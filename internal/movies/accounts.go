package movies

import (
	"context"
	"log/slog"
	"time"

	"github.com/shyper/autodownloadarr/internal/mediaserver"
	"github.com/shyper/autodownloadarr/internal/store"
)

// ItemIndex is the library of one account, indexed by rating key and by a
// local path (a movie folder or an episode file).
type ItemIndex struct {
	ByKey  map[string]mediaserver.Item
	ByPath map[string]mediaserver.Item
}

// Lookup finds the item of a local path, falling back to the media key.
func (ix *ItemIndex) Lookup(path, mediaKey string) (mediaserver.Item, bool) {
	if it, ok := ix.ByPath[path]; ok {
		return it, true
	}
	it, ok := ix.ByKey[mediaKey]
	return it, ok
}

// AccountLibraries reads the library once per account during one poll. The
// watch state of a request comes from the media profile of its requester.
type AccountLibraries struct {
	svc    *Services
	target *Target
	log    *slog.Logger
	fetch  func(context.Context, mediaserver.Server) ([]mediaserver.Item, error)
	path   func(mediaserver.Item, string) string // item + remote file → local index path

	users    map[int64]string
	indexes  map[string]*ItemIndex
	profiles []store.MediaProfile // assigned profiles, loaded on demand
}

// NewAccountLibraries prepares a poll. fetch lists the library of a client;
// path converts a remote file of an item into the local index path.
func NewAccountLibraries(svc *Services, target *Target, log *slog.Logger,
	fetch func(context.Context, mediaserver.Server) ([]mediaserver.Item, error),
	path func(mediaserver.Item, string) string,
) *AccountLibraries {
	return &AccountLibraries{svc: svc, target: target, log: log, fetch: fetch, path: path,
		users: map[int64]string{}, indexes: map[string]*ItemIndex{}}
}

// Account returns the account key for the requests of a user.
func (a *AccountLibraries) Account(ctx context.Context, userID int64) (string, error) {
	if key, ok := a.users[userID]; ok {
		return key, nil
	}
	key, err := a.svc.AccountForUser(ctx, userID)
	if err != nil {
		return "", err
	}
	a.users[userID] = key
	return key, nil
}

// Index returns the library as the account sees it. A failing profile is
// recorded on the profile and yields nil, so its requests are skipped this
// time; a failing default account is an error, as before profiles existed.
func (a *AccountLibraries) Index(ctx context.Context, key string) (*ItemIndex, error) {
	if ix, ok := a.indexes[key]; ok {
		return ix, nil
	}
	items, err := a.load(ctx, key)
	profileID := store.ProfileIDFromAccountKey(key)
	if err != nil {
		if key == "" {
			return nil, err
		}
		a.log.Warn("read watch state of media profile failed", "profile", profileID, "err", err)
		if serr := a.svc.Store.SetMediaProfileError(ctx, profileID, err.Error()); serr != nil {
			return nil, serr
		}
		a.indexes[key] = nil
		return nil, nil
	}
	if profileID > 0 {
		if err := a.svc.Store.SetMediaProfileError(ctx, profileID, ""); err != nil {
			return nil, err
		}
	}
	ix := &ItemIndex{ByKey: map[string]mediaserver.Item{}, ByPath: map[string]mediaserver.Item{}}
	for _, it := range items {
		ix.ByKey[it.Key] = it
		for _, f := range it.Files {
			ix.ByPath[a.path(it, f)] = it
		}
	}
	a.indexes[key] = ix
	return ix, nil
}

func (a *AccountLibraries) load(ctx context.Context, key string) ([]mediaserver.Item, error) {
	srv, err := a.target.ForAccount(ctx, key)
	if err != nil {
		return nil, err
	}
	return a.fetch(ctx, srv)
}

// SwitchAccount keeps a watch baseline tied to the account it was read
// from. When the account whose watch state counts changes (a profile was
// assigned or removed), a watched item stays watched and an unwatched one
// takes a new baseline from the new account.
func SwitchAccount(base *store.WatchBase, key string, watched bool, it mediaserver.Item) {
	if base.Account == key {
		return
	}
	switch {
	case !base.Set:
	case watched:
		// One completed viewing below the current count, as for watched movies.
		*base = store.WatchBase{Set: true, Views: it.ViewCount - 1, Offset: it.ViewOffset.Milliseconds()}
	default:
		*base = store.WatchBase{}
	}
	base.Account = key
}

// Participants returns the users whose watch state counts for a title: its
// active viewers in the order they joined, or the requester (0: the default
// account) when nobody watches it any more. primary decides when the title
// counts as watched: the requester while they watch it, else the first
// viewer.
func Participants(viewers []store.Viewer, requester int64) (users []int64, primary int64) {
	for _, v := range viewers {
		if v.Active() {
			users = append(users, v.UserID)
		}
	}
	if len(users) == 0 {
		return []int64{requester}, requester
	}
	for _, u := range users {
		if u == requester {
			return users, requester
		}
	}
	return users, users[0]
}

// Detected is a profile that started watching an item without being one of
// its viewers.
type Detected struct {
	UserID  int64
	Account string
	Item    mediaserver.Item
}

// Detect finds assigned profiles that played an item after its file
// appeared (since) while their user is not a viewer (known holds every
// user with a viewer row, also those who left).
func (a *AccountLibraries) Detect(ctx context.Context, known map[int64]bool, since time.Time,
	lookup func(*ItemIndex) (mediaserver.Item, bool),
) ([]Detected, error) {
	if since.IsZero() {
		return nil, nil // unknown when the file appeared: nothing to tell apart
	}
	if a.profiles == nil {
		list, err := a.svc.Store.MediaProfiles(ctx)
		if err != nil {
			return nil, err
		}
		a.profiles = []store.MediaProfile{}
		for _, p := range list {
			if p.UserID > 0 {
				a.profiles = append(a.profiles, p)
			}
		}
	}
	var out []Detected
	for _, p := range a.profiles {
		if known[p.UserID] {
			continue
		}
		account, err := a.Account(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		if account == "" && !p.Owner {
			continue // the profile cannot be read: its plays are not its own
		}
		ix, err := a.Index(ctx, account)
		if err != nil {
			return nil, err
		}
		if ix == nil {
			continue
		}
		it, ok := lookup(ix)
		if !ok || it.LastViewedAt.Before(since.Truncate(time.Second)) {
			continue
		}
		out = append(out, Detected{UserID: p.UserID, Account: account, Item: it})
	}
	return out, nil
}

// PrepareBase gets a viewer's baseline ready for an observation with the
// account key: a viewer who had seen the item before a new file appeared
// stays at "seen", and a changed account takes a new baseline.
func PrepareBase(p *store.Progress, account string, it mediaserver.Item) {
	if !p.Base.Set && p.WatchedAt != nil {
		p.Base = store.WatchBase{Set: true, Views: it.ViewCount - 1, Offset: it.ViewOffset.Milliseconds(), Account: account}
		return
	}
	SwitchAccount(&p.Base, account, p.WatchedAt != nil, it)
}
