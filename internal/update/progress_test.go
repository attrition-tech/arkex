package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadProgress(t *testing.T) {
	for _, tc := range []struct {
		name               string
		total, limit, want int64
		fail               bool
	}{
		{"known", 8193, 10000, 8193, false},
		{"unknown", -1, 10000, 8193, false},
		{"truncated", 9000, 10000, 8193, true},
		{"limited", -1, 1024, 1025, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.total >= 0 {
					w.Header().Set("Content-Length", fmt.Sprint(tc.total))
				}
				w.(http.Flusher).Flush()
				_, _ = fmt.Fprint(w, strings.Repeat("x", 8193))
			}))
			defer srv.Close()
			var last int64 = -1
			c := &Client{}
			_, err := c.get(context.Background(), srv.URL, tc.limit, func(n, total int64) {
				if n <= last || total != tc.total {
					t.Errorf("progress (%d, %d), previous %d", n, total, last)
				}
				last = n
			})
			if (err != nil) != tc.fail || last != tc.want {
				t.Fatalf("received %d, err %v", last, err)
			}
		})
	}
}
