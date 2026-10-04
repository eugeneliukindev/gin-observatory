// Package dependencies hands the handlers what the application was built with, through the gin
// context, as FastAPI's Depends hands it through the request.
package dependencies

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"go-observatory/internal/cache"
	"go-observatory/internal/config"
)

const key = "dependencies"

// Post is a post as JSONPlaceholder and the API speak of it.
type Post struct {
	ID     int64  `json:"id"`
	UserID int64  `json:"userId"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// Dependencies are what the application was built with.
type Dependencies struct {
	// The JSONPlaceholder client, with its base address.
	HTTP *http.Client
	// The posts recently read, by key.
	Cache *cache.MemoryCache[Post]
	// The traced connection opened for the lifetime of the application.
	Database *sql.DB
	// The settings the application was built with.
	Settings config.Settings
}

// Provide puts the dependencies into every request's context.
func Provide(deps *Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(key, deps)
		c.Next()
	}
}

// From returns the dependencies of the request.
func From(c *gin.Context) *Dependencies {
	deps, ok := c.MustGet(key).(*Dependencies)
	if !ok {
		panic("dependencies: Provide is not on the route")
	}
	return deps
}
