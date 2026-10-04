// Package router holds the handlers: they have no business logic, they only touch what shows up in
// Grafana — the cache, SQLite, JSONPlaceholder and the CPU.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"go-observatory/internal/dependencies"
	"go-observatory/internal/enums"
	"go-observatory/internal/middleware"
)

const (
	savePost = `
INSERT INTO posts (id, user_id, title, body) VALUES (?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET saved_at = CURRENT_TIMESTAMP`
	addPostQuery = `INSERT INTO posts (user_id, title, body) VALUES (?, ?, ?) RETURNING id`
	recentPosts  = `SELECT id, user_id, title, body FROM posts ORDER BY saved_at DESC LIMIT 20`
)

const (
	// Short on purpose: cache misses happen even under light traffic.
	cacheTTL          = 30 * time.Second
	reportPrimesBelow = 500_000
)

var tracer = otel.Tracer("go-observatory/internal/router")

// Register adds the routes to the engine.
func Register(engine *gin.Engine) {
	engine.GET("/", showPage)
	engine.GET("/health", checkHealth)
	engine.GET("/api/posts/:post_id", getPost)
	engine.GET("/api/posts", listPosts)
	engine.POST("/api/posts", addPost)
	engine.GET("/api/cpu", burnCPU)
	engine.GET("/api/report/:post_id", buildReport)
	engine.GET("/api/fail", fail)
}

// unprocessable answers a request whose path, query or body did not validate: a 422, as FastAPI's.
func unprocessable(c *gin.Context, err error) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
}

// showPage serves the page.
func showPage(c *gin.Context) {
	faro := dependencies.From(c).Settings.Obs
	c.HTML(http.StatusOK, "index.html", gin.H{"FaroURL": faro.Faro.CollectorURL, "Environment": faro.Environment})
}

// checkHealth answers the liveness probe.
func checkHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

type postPath struct {
	PostID int64 `uri:"post_id" binding:"required"`
}

// getPost reads a post: from the cache, on a miss from JSONPlaceholder, then saves and caches it.
func getPost(c *gin.Context) {
	var path postPath
	if err := c.ShouldBindUri(&path); err != nil {
		unprocessable(c, err)
		return
	}
	post, found, err := readPost(c.Request.Context(), dependencies.From(c), path.PostID)
	switch {
	case err != nil:
		middleware.Unhandled(c, err)
	case !found:
		c.JSON(http.StatusNotFound, gin.H{"detail": fmt.Sprintf("post %d not found", path.PostID)})
	default:
		c.JSON(http.StatusOK, post)
	}
}

func readPost(ctx context.Context, deps *dependencies.Dependencies, id int64) (dependencies.Post, bool, error) {
	key := fmt.Sprintf("post:%d", id)
	if post, ok := deps.Cache.Get(ctx, key); ok {
		return post, true, nil
	}
	var post dependencies.Post
	status, err := getJSON(ctx, deps.HTTP, fmt.Sprintf("/posts/%d", id), &post)
	if err != nil || status == http.StatusNotFound {
		return dependencies.Post{}, false, err
	}
	// A local file answers in microseconds: the query runs right in the handler.
	if _, err := deps.Database.ExecContext(ctx, savePost, post.ID, post.UserID, post.Title, post.Body); err != nil {
		return dependencies.Post{}, false, fmt.Errorf("save post %d: %w", id, err)
	}
	deps.Cache.Set(ctx, key, post, cacheTTL)
	slog.InfoContext(ctx, "post fetched from JSONPlaceholder and cached", "post_id", id)
	return post, true, nil
}

// getJSON reads path from JSONPlaceholder into `into`; a 404 is returned as a status, not an error.
func getJSON(ctx context.Context, client *http.Client, path string, into any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", path, err)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", path, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		if err := json.NewDecoder(response.Body).Decode(into); err != nil {
			return response.StatusCode, fmt.Errorf("GET %s: %w", path, err)
		}
		return response.StatusCode, nil
	case http.StatusNotFound:
		return response.StatusCode, nil
	default:
		return response.StatusCode, fmt.Errorf("GET %s: status %d", path, response.StatusCode)
	}
}

// listPosts lists the most recently read posts.
func listPosts(c *gin.Context) {
	rows, err := dependencies.From(c).Database.QueryContext(c.Request.Context(), recentPosts)
	if err != nil {
		middleware.Unhandled(c, fmt.Errorf("recent posts: %w", err))
		return
	}
	defer rows.Close()
	posts := []dependencies.Post{}
	for rows.Next() {
		var post dependencies.Post
		if err := rows.Scan(&post.ID, &post.UserID, &post.Title, &post.Body); err != nil {
			middleware.Unhandled(c, fmt.Errorf("recent posts: %w", err))
			return
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		middleware.Unhandled(c, fmt.Errorf("recent posts: %w", err))
		return
	}
	c.JSON(http.StatusOK, posts)
}

// postDraft is a post as the client writes it: the id is given by the database.
type postDraft struct {
	UserID int64  `json:"user_id" binding:"required,gt=0"`
	Title  string `json:"title"   binding:"required"`
	Body   string `json:"body"    binding:"required"`
}

// addPost saves the post sent in the request body.
func addPost(c *gin.Context) {
	var draft postDraft
	if err := c.ShouldBindJSON(&draft); err != nil {
		unprocessable(c, err)
		return
	}
	post := dependencies.Post{UserID: draft.UserID, Title: draft.Title, Body: draft.Body}
	err := dependencies.From(c).Database.QueryRowContext(c.Request.Context(), addPostQuery,
		post.UserID, post.Title, post.Body).Scan(&post.ID)
	if err != nil {
		middleware.Unhandled(c, fmt.Errorf("add post: %w", err))
		return
	}
	slog.InfoContext(c.Request.Context(), "post created", "post_id", post.ID)
	c.JSON(http.StatusCreated, post)
}

// Up to a few seconds of trial division: enough to stand out in a profile.
type cpuQuery struct {
	Below int `form:"below,default=1000000" binding:"min=2,max=20000000"`
}

// burnCPU counts the primes below `below`.
func burnCPU(c *gin.Context) {
	var query cpuQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		unprocessable(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"primes": countPrimes(c.Request.Context(), query.Below)})
}

// buildReport does everything at once: the post and its comments in parallel, then CPU work.
func buildReport(c *gin.Context) {
	var path postPath
	if err := c.ShouldBindUri(&path); err != nil {
		unprocessable(c, err)
		return
	}
	deps := dependencies.From(c)
	var (
		post     dependencies.Post
		found    bool
		comments []struct{}
	)
	group, ctx := errgroup.WithContext(c.Request.Context())
	group.Go(func() (err error) {
		post, found, err = readPost(ctx, deps, path.PostID)
		return err
	})
	group.Go(func() error {
		_, err := getJSON(ctx, deps.HTTP, fmt.Sprintf("/posts/%d/comments", path.PostID), &comments)
		return err
	})
	if err := group.Wait(); err != nil {
		middleware.Unhandled(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"detail": fmt.Sprintf("post %d not found", path.PostID)})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"title":    post.Title,
		"comments": len(comments),
		"primes":   countPrimes(c.Request.Context(), reportPrimesBelow),
	})
}

type failQuery struct {
	Kind enums.Failure `form:"kind,default=runtime" binding:"oneof=runtime parse decode timeout permission"`
}

// fail gives up with an unhandled error of the kind asked for.
func fail(c *gin.Context) {
	var query failQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		unprocessable(c, err)
		return
	}
	middleware.Unhandled(c, failure(c.Request.Context(), query.Kind))
}

// failure returns the error a failure of this kind gives up with, each from a real operation.
func failure(ctx context.Context, kind enums.Failure) error {
	switch kind {
	case enums.FailureRuntime:
		// Indexing past the end of a slice: a runtime panic, runtime.boundsError.
		var routes []string
		_ = routes[len(kind)] //nolint:gosec // the panic is the point
	case enums.FailureParse:
		// A number that is not one: *strconv.NumError.
		_, err := strconv.Atoi("forty-two")
		return err
	case enums.FailureDecode:
		// A title that is a number: *json.UnmarshalTypeError.
		var post dependencies.Post
		return json.Unmarshal([]byte(`{"title": 7}`), &post)
	case enums.FailureTimeout:
		// Waiting past its own deadline: context.DeadlineExceeded.
		ctx, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		<-ctx.Done()
		return ctx.Err()
	case enums.FailurePermission:
		// A directory only its owner may read: *fs.PathError.
		if _, err := os.ReadDir("/root"); err != nil {
			return err
		}
		// Run as root, the directory opens: the error is the one any other user gets.
		return &fs.PathError{Op: "open", Path: "/root", Err: fs.ErrPermission}
	}
	return fmt.Errorf("failure %q", kind)
}

// countPrimes counts the primes below `below` by trial division — expensive on purpose, and in a
// span of its own, so the profile of that span is exactly this work.
func countPrimes(ctx context.Context, below int) int {
	_, span := tracer.Start(ctx, "count primes", trace.WithAttributes(attribute.Int("primes.below", below)))
	defer span.End()
	count := 0
	for number := 2; number < below; number++ {
		if isPrime(number) {
			count++
		}
	}
	return count
}

func isPrime(number int) bool {
	for divisor := 2; divisor*divisor <= number; divisor++ {
		if number%divisor == 0 {
			return false
		}
	}
	return true
}
