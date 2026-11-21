package httpin

import (
	"context"
	"net/http"
)

type ctxKey int

const userKey ctxKey = iota

func withUserID(r *http.Request, uid string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userKey, uid))
}

func userID(r *http.Request) string {
	v, _ := r.Context().Value(userKey).(string)
	return v
}
