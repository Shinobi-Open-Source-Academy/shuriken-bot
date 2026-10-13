package activity

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sos-academy/shuriken/internal/store"
)

// Recorder enriches normalized activities with platform identities and
// persists them.
type Recorder struct {
	store  store.Store
	log    *slog.Logger
	dryRun bool
}

// NewRecorder builds a Recorder. When dryRun is true nothing is written and
// every activity is logged instead.
func NewRecorder(s store.Store, log *slog.Logger, dryRun bool) *Recorder {
	return &Recorder{store: s, log: log, dryRun: dryRun}
}

// Record resolves the academy platform user and project for each activity and
// stores it.
//
// An activity whose GitHub user has no platform account is still recorded, under
// the GitHub login, with Unlinked set. Shuriken never creates platform
// accounts, so the alternative would be losing the contribution entirely.
func (r *Recorder) Record(ctx context.Context, activities []*store.Activity) error {
	var errs []error
	for _, a := range activities {
		if err := r.enrich(ctx, a); err != nil {
			// Enrichment is best effort: a failed lookup must not lose the
			// activity, it just stays unlinked.
			r.log.WarnContext(ctx, "activity enrichment failed, recording unlinked",
				slog.String("dedupeKey", a.DedupeKey), slog.Any("error", err))
			a.Unlinked = true
		}

		if r.dryRun {
			r.log.InfoContext(ctx, "DRY RUN would record activity",
				slog.String("type", string(a.Type)),
				slog.String("dedupeKey", a.DedupeKey),
				slog.String("actor", a.Actor.Login),
				slog.String("repo", a.Repo.FullName),
				slog.String("url", a.Subject.URL),
				slog.Bool("unlinked", a.Unlinked),
				slog.String("platformUserId", a.UserID),
				slog.String("projectId", a.ProjectID),
				slog.Time("occurredAt", a.OccurredAt),
			)
			continue
		}

		switch err := r.store.RecordActivity(ctx, a); {
		case err == nil:
			r.log.InfoContext(ctx, "activity recorded",
				slog.String("type", string(a.Type)),
				slog.String("dedupeKey", a.DedupeKey),
				slog.String("actor", a.Actor.Login),
				slog.Bool("unlinked", a.Unlinked))
		case errors.Is(err, store.ErrDuplicate):
			// A redelivery, or the same logical event arriving twice. Already
			// recorded, so this is success.
			r.log.DebugContext(ctx, "activity already recorded",
				slog.String("dedupeKey", a.DedupeKey))
		default:
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Recorder) enrich(ctx context.Context, a *store.Activity) error {
	a.Unlinked = true

	if r.store == nil {
		return nil
	}

	user, err := r.store.FindPlatformUser(ctx, a.Actor.GithubID, a.Actor.Login)
	if err != nil {
		return err
	}
	if user != nil {
		a.UserID = user.ID
		a.Unlinked = false
	}

	if a.Repo.FullName != "" {
		projectID, err := r.store.ProjectIDForRepo(ctx, a.Repo.FullName)
		if err != nil {
			return err
		}
		a.ProjectID = projectID
	}
	return nil
}
