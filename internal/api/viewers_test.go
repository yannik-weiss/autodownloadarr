package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/shyper/autodownloadarr/internal/store"
)

func TestViewerEndpoints(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	e.completeSetup(t, c)
	ctx := context.Background()
	resp, body := e.do(t, c, "POST", "/api/v1/users", map[string]any{"username": "bob", "password": "longenough", "role": "user"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: %d %v", resp.StatusCode, body)
	}
	bob := int64(body["id"].(float64))
	mv, err := e.store.CreateMovie(ctx, store.Movie{TMDBID: 42, Title: "Film", State: store.MovieAvailable}, 0) // the admin's
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/movies/" + strconv.FormatInt(mv.ID, 10)

	bc := e.client(t)
	if resp, body := e.do(t, bc, "POST", "/api/v1/auth/login", map[string]string{"username": "bob", "password": "longenough"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	resp, body = e.do(t, bc, "POST", path+"/watch", nil)
	if list, _ := body["viewers"].([]any); resp.StatusCode != http.StatusOK || len(list) != 2 {
		t.Fatalf("join: %d %v", resp.StatusCode, body)
	}
	// A viewer may not delete the movie; only its owner or an admin.
	if resp, _ := e.do(t, bc, "DELETE", path, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer delete: %d", resp.StatusCode)
	}
	resp, body = e.do(t, bc, "DELETE", path+"/watch", nil)
	if list, _ := body["viewers"].([]any); resp.StatusCode != http.StatusOK || len(list) != 1 {
		t.Fatalf("leave: %d %v", resp.StatusCode, body)
	}
	// Removing someone else is for administrators.
	if _, err := e.store.AddViewer(ctx, store.Viewer{Kind: store.ViewerMovie, TitleID: mv.ID, UserID: bob}, true); err != nil {
		t.Fatal(err)
	}
	removePath := path + "/viewers/" + strconv.FormatInt(bob, 10)
	if resp, _ := e.do(t, bc, "DELETE", path+"/viewers/1", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin removal: %d", resp.StatusCode)
	}
	resp, body = e.do(t, c, "DELETE", removePath, nil)
	if list, _ := body["viewers"].([]any); resp.StatusCode != http.StatusOK || len(list) != 1 {
		t.Fatalf("admin removal: %d %v", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, c, "GET", "/api/v1/movies/999/viewers", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown movie: %d", resp.StatusCode)
	}
}
