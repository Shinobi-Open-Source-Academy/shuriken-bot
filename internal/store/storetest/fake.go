// Package storetest provides an in-memory store.Store for tests in other
// packages. It is not used by the running service.
package storetest

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/sos-academy/shuriken/internal/store"
)

// Fake is an in-memory store.Store. It enforces the same uniqueness
// constraints as the real indexes, so idempotency can be tested without a
// database.
type Fake struct {
	mu sync.Mutex

	// Deliveries seen, keyed by delivery id.
	Deliveries map[string]string
	// Activities recorded, in order.
	Activities []*store.Activity
	// Runs claimed, keyed by trigger key.
	Runs map[string]*store.ReviewRun
	// Findings saved, in order.
	Findings []*store.Finding
	// Users is the GitHub id to platform user mapping.
	Users map[int64]*store.PlatformUser
	// UsersByLogin is the lowercased login fallback mapping.
	UsersByLogin map[string]*store.PlatformUser
	// Projects maps "owner/name" to a project id.
	Projects map[string]string
	// Revealed records ids passed to MarkFindingsRevealed.
	Revealed []string

	// Err, when set, is returned by every method. For failure-path tests.
	Err error

	dedupe map[string]bool
}

// New builds an empty Fake.
func New() *Fake {
	return &Fake{
		Deliveries:   map[string]string{},
		Runs:         map[string]*store.ReviewRun{},
		Users:        map[int64]*store.PlatformUser{},
		UsersByLogin: map[string]*store.PlatformUser{},
		Projects:     map[string]string{},
		dedupe:       map[string]bool{},
	}
}

// AddUser registers a linked platform user.
func (f *Fake) AddUser(id string, githubID int64, login string) {
	u := &store.PlatformUser{ID: id, GithubID: githubID, GithubLogin: login}
	f.Users[githubID] = u
	f.UsersByLogin[strings.ToLower(login)] = u
}

func (f *Fake) MarkDelivery(_ context.Context, deliveryID, event string) (bool, error) {
	if f.Err != nil {
		return false, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, seen := f.Deliveries[deliveryID]; seen {
		return false, nil
	}
	f.Deliveries[deliveryID] = event
	return true, nil
}

func (f *Fake) RecordActivity(_ context.Context, a *store.Activity) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dedupe[a.DedupeKey] {
		return store.ErrDuplicate
	}
	f.dedupe[a.DedupeKey] = true
	copied := *a
	f.Activities = append(f.Activities, &copied)
	return nil
}

func (f *Fake) FindPlatformUser(_ context.Context, githubID int64, login string) (*store.PlatformUser, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.Users[githubID]; ok {
		return u, nil
	}
	if u, ok := f.UsersByLogin[strings.ToLower(login)]; ok {
		return u, nil
	}
	return nil, nil
}

func (f *Fake) ProjectIDForRepo(_ context.Context, fullName string) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Projects[strings.ToLower(fullName)], nil
}

func (f *Fake) ClaimReviewRun(_ context.Context, run *store.ReviewRun) (bool, error) {
	if f.Err != nil {
		return false, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	key := TriggerKey(run)
	if _, taken := f.Runs[key]; taken {
		return false, nil
	}
	copied := *run
	copied.ID = "run-" + strconv.Itoa(len(f.Runs)+1)
	f.Runs[key] = &copied
	run.ID = copied.ID
	return true, nil
}

func (f *Fake) CompleteReviewRun(_ context.Context, run *store.ReviewRun) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := *run
	f.Runs[TriggerKey(run)] = &copied
	return nil
}

func (f *Fake) SaveFindings(_ context.Context, findings []*store.Finding) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Findings = append(f.Findings, findings...)
	return nil
}

func (f *Fake) LatestReviewFindings(_ context.Context, prGithubID int64) ([]*store.Finding, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var newest int64
	for _, fd := range f.Findings {
		if fd.PR.GithubID == prGithubID && fd.ReviewID > newest {
			newest = fd.ReviewID
		}
	}
	var out []*store.Finding
	for _, fd := range f.Findings {
		if fd.PR.GithubID == prGithubID && fd.ReviewID == newest {
			out = append(out, fd)
		}
	}
	return out, nil
}

func (f *Fake) MarkFindingsRevealed(_ context.Context, ids []string) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Revealed = append(f.Revealed, ids...)
	return nil
}

func (f *Fake) Close(context.Context) error { return nil }

// TriggerKey mirrors the real store's idempotency key for a review trigger.
func TriggerKey(run *store.ReviewRun) string {
	if run.CommandCommentID != 0 {
		return "comment:" + strconv.FormatInt(run.CommandCommentID, 10)
	}
	return "label:" + strconv.FormatInt(run.PR.GithubID, 10) + ":" + run.PR.HeadSHA
}

// ActivityCount returns how many activities were recorded.
func (f *Fake) ActivityCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Activities)
}
