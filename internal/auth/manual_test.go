package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeTokenServer answers /o/token/ and records the form it was sent.
func fakeTokenServer(t *testing.T, form *url.Values) Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/o/token/" {
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		*form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"acc","refresh_token":"ref","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	return Config{AccountURL: srv.URL, ClientID: "cid", RedirectURI: "https://app.example/cb", HTTP: srv.Client()}
}

func TestManualLoginAcceptsAURLOrACode(t *testing.T) {
	for name, paste := range map[string]func(state string) string{
		"url":   func(s string) string { return "https://app.example/cb?code=pasted&state=" + s },
		"query": func(s string) string { return "code=pasted&state=" + s },
		"code":  func(string) string { return "pasted" },
	} {
		t.Run(name, func(t *testing.T) {
			var form url.Values
			c := fakeTokenServer(t, &form)
			var state string
			tok, err := c.ManualLogin(context.Background(),
				func(u string) { au, _ := url.Parse(u); state = au.Query().Get("state") },
				func() (string, error) { return " " + paste(state) + "\n", nil })
			if err != nil || tok.AccessToken != "acc" {
				t.Fatalf("got %+v, %v", tok, err)
			}
			if form.Get("code") != "pasted" || form.Get("redirect_uri") != "https://app.example/cb" {
				t.Fatalf("token request %v", form)
			}
		})
	}
}

func TestManualLoginRejectsBadPastes(t *testing.T) {
	for _, paste := range []string{"", "https://app.example/cb?code=x&state=forged", "https://app.example/cb?state=s"} {
		var form url.Values
		c := fakeTokenServer(t, &form)
		_, err := c.ManualLogin(context.Background(), func(string) {}, func() (string, error) { return paste, nil })
		if err == nil || form != nil {
			t.Errorf("paste %q: err %v, redeemed %v", paste, err, form)
		}
	}
}
