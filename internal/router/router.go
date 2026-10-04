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
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"go-observatory/internal/dependencies"
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

// badRequest answers a request whose path, query or body did not bind: the client's to fix.
func badRequest(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

// notFound answers a request for a post there is none of.
func notFound(c *gin.Context, id int64) {
	c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("post %d not found", id)})
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
		badRequest(c, err)
		return
	}
	post, found, err := readPost(c.Request.Context(), dependencies.From(c), path.PostID)
	switch {
	case err != nil:
		_ = c.Error(err)
	case !found:
		notFound(c, path.PostID)
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
		_ = c.Error(fmt.Errorf("recent posts: %w", err))
		return
	}
	defer rows.Close()
	posts := []dependencies.Post{}
	for rows.Next() {
		var post dependencies.Post
		if err := rows.Scan(&post.ID, &post.UserID, &post.Title, &post.Body); err != nil {
			_ = c.Error(fmt.Errorf("recent posts: %w", err))
			return
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		_ = c.Error(fmt.Errorf("recent posts: %w", err))
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
		badRequest(c, err)
		return
	}
	post := dependencies.Post{UserID: draft.UserID, Title: draft.Title, Body: draft.Body}
	err := dependencies.From(c).Database.QueryRowContext(c.Request.Context(), addPostQuery,
		post.UserID, post.Title, post.Body).Scan(&post.ID)
	if err != nil {
		_ = c.Error(fmt.Errorf("add post: %w", err))
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
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"primes": countPrimes(c.Request.Context(), query.Below)})
}

// buildReport does everything at once: the post and its comments in parallel, then CPU work.
func buildReport(c *gin.Context) {
	var path postPath
	if err := c.ShouldBindUri(&path); err != nil {
		badRequest(c, err)
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
		_ = c.Error(err)
		return
	}
	if !found {
		notFound(c, path.PostID)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"title":    post.Title,
		"comments": len(comments),
		"primes":   countPrimes(c.Request.Context(), reportPrimesBelow),
	})
}

// failure is a way /api/fail gives up: a bug panics, an operation that did not succeed returns an
// error. Each has a Go type of its own, error.type on the metrics.
type failure string

const (
	failureIndex      failure = "index"      // a panic: runtime.boundsError
	failureNil        failure = "nil"        // a panic: runtime.errorString
	failureAssertion  failure = "assertion"  // a panic: *runtime.TypeAssertionError
	failureMap        failure = "map"        // a panic: runtime.plainError
	failureTimeout    failure = "timeout"    // an error: context.deadlineExceededError, a 504
	failurePermission failure = "permission" // an error: *fs.PathError
)

type failQuery struct {
	Kind failure `form:"kind,default=index" binding:"oneof=index nil assertion map timeout permission"`
}

// fail gives up the way asked for: by a panic or by an error handed to the error middleware.
func fail(c *gin.Context) {
	var query failQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		badRequest(c, err)
		return
	}
	if err := giveUp(c.Request.Context(), query.Kind); err != nil {
		_ = c.Error(err)
	}
}

// giveUp fails as kind does, each by the mistake or the operation that fails so in real code.
func giveUp(ctx context.Context, kind failure) error {
	switch kind {
	case failureIndex:
		// The first of none.
		var routes []string
		_ = routes[len(kind)] //nolint:gosec // the panic is the point
	case failureNil:
		// A pointer read before anything was assigned to it.
		var post *dependencies.Post
		_ = post.Title
	case failureAssertion:
		// An interface asserted to a type it does not hold.
		var payload any = len(kind)
		_ = payload.(string) //nolint:forcetypeassert // the panic is the point
	case failureMap:
		// A write to a map never made.
		var seen map[failure]bool
		seen[kind] = true //nolint:staticcheck // the panic is the point
	case failureTimeout:
		// Waiting past its own deadline.
		ctx, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		<-ctx.Done()
		return fmt.Errorf("wait for the report: %w", ctx.Err())
	case failurePermission:
		// A directory only its owner may read; run as root, it opens, so the error is the one
		// any other user gets.
		err := error(&fs.PathError{Op: "open", Path: "/root", Err: fs.ErrPermission})
		if _, readErr := os.ReadDir("/root"); readErr != nil {
			err = readErr
		}
		return fmt.Errorf("list reports: %w", err)
	}
	return nil
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
