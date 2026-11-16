// Package access defines the same ownership policy for HTTP, MCP and bots.
package access

import (
	"context"
	"errors"

	"github.com/shyper/autodownloadarr/internal/auth"
	"github.com/shyper/autodownloadarr/internal/store"
)

var ErrForbidden = errors.New("forbidden")

func CanManage(user auth.User, owner int64) bool {
	return user.ID > 0 && (user.Role == auth.RoleAdmin || (owner > 0 && owner == user.ID))
}

func Check(ctx context.Context, st *store.Store, user auth.User, kind string, id int64) error {
	var owner int64
	switch kind {
	case "movie":
		item, err := st.Movie(ctx, id)
		if err != nil {
			return err
		}
		owner = item.RequestedBy
	case "series":
		item, err := st.Series(ctx, id)
		if err != nil {
			return err
		}
		owner = item.RequestedBy
	case "episode":
		ep, err := st.Episode(ctx, id)
		if err != nil {
			return err
		}
		item, err := st.Series(ctx, ep.SeriesID)
		if err != nil {
			return err
		}
		owner = item.RequestedBy
	default:
		return ErrForbidden
	}
	if !CanManage(user, owner) {
		return ErrForbidden
	}
	return nil
}

// MayRestore reports whether a user may bring back the removed files of a
// title (kind store.ViewerMovie or store.ViewerSeries): its owner, an
// administrator or one of its viewers.
func MayRestore(ctx context.Context, st *store.Store, user auth.User, kind string, id, owner int64) bool {
	if CanManage(user, owner) {
		return true
	}
	viewer, err := st.IsViewer(ctx, kind, id, user.ID)
	return err == nil && viewer
}
