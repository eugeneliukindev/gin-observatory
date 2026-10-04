package middleware

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// PanicError is a panic recovered in a handler: a bug, with the stack it happened on. Returned
// errors carry no stack — the context fmt.Errorf wraps them in says where they come from.
type PanicError struct {
	Value any
	Stack string
}

func (p *PanicError) Error() string { return fmt.Sprint(p.Value) }

// ErrorType names the panic by the value it panicked with, as the SDK names a panic that ends a
// span: runtime.boundsError, not the PanicError around it.
func (p *PanicError) ErrorType() string {
	if err, ok := p.Value.(error); ok {
		return semconv.ErrorType(err).Value.AsString()
	}
	return fmt.Sprintf("%T", p.Value)
}

// Recovery turns a panic in a handler into a 500 and a PanicError on the request, for Access to
// report. A broken connection is gin's to handle: it is no bug, and there is no one to answer.
func Recovery() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, recovered any) {
		// Still on the panicking goroutine's stack: the trace leads to the line that panicked.
		_ = c.Error(&PanicError{Value: recovered, Stack: string(debug.Stack())})
		answer(c, http.StatusInternalServerError)
	})
}

// answer ends the request with a status and its text only: what failed stays in the server's log.
func answer(c *gin.Context, status int) {
	c.AbortWithStatusJSON(status, gin.H{"error": http.StatusText(status)})
}
