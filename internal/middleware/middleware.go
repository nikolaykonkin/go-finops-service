package middleware

import (
	"log"
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecorder запоминает код ответа, чтобы его можно было залогировать
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// LoggingMiddleware логирует метод, путь, код ответа и время выполнения запроса
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}

// RecoveryMiddleware перехватывает panic в обработчике, логирует ее вместе со
// стеком и возвращает клиенту 500
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler — штатный способ прервать ответ, его пробрасываем
			if rec == http.ErrAbortHandler {
				panic(rec)
			}

			log.Printf("panic: %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal server error"}` + "\n"))
		}()

		next.ServeHTTP(w, r)
	})
}
