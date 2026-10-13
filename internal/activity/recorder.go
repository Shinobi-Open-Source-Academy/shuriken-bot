package activity

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sos-academy/shuriken/internal/publisher"
	"github.com/sos-academy/shuriken/internal/store"
)

// Recorder persists normalized activities and queues them for delivery.
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

// Record stores each activity and enqueues an outbound event for it.
//
// There is no identity resolution step. Shuriken records the GitHub actor and
// nothing else; mapping that to an account is the consumer's job, done at read
// time against its own records. That is what lets one deployment serve
// several projects, and it means Shuriken holds no credential for anyone
// else's user data.
//
// The write and the enqueue are deliberately ordered: an activity that is
// stored but not yet queued is recoverable, whereas an event queued for
// something that was never stored is a lie. A duplicate activity still
// re-enqueues, which is safe because the outbox is keyed on the same dedupe
// key and ignores repeats.
func (r *Recorder) Record(ctx context.Context, activities []*store.Activity) error {
	var errs []error

	for _, a := range activities {
		if r.dryRun {
			r.log.InfoContext(ctx, "DRY RUN would record activity",
				slog.String("type", string(a.Type)),
				slog.String("dedupeKey", a.DedupeKey),
				slog.String("actor", a.Actor.Login),
				slog.Int64("actorGithubId", a.Actor.GithubID),
				slog.String("repo", a.Repo.FullName),
				slog.String("url", a.Subject.URL),
				slog.Time("occurredAt", a.OccurredAt))

			// Show the exact event a consumer would receive. This works with
			// no database configured, which is what makes dry run useful to
			// someone implementing the consumer side.
			if event, err := publisher.ActivityEvent(a); err == nil {
				r.log.InfoContext(ctx, "DRY RUN would publish event",
					slog.String("eventType", event.Type),
					slog.String("idempotencyKey", event.IdempotencyKey),
					slog.String("payload", string(event.Payload)))
			}
			continue
		}
		if r.store == nil {
			continue
		}

		switch err := r.store.RecordActivity(ctx, a); {
		case err == nil:
			r.log.InfoContext(ctx, "activity recorded",
				slog.String("type", string(a.Type)),
				slog.String("dedupeKey", a.DedupeKey),
				slog.String("actor", a.Actor.Login))
		case errors.Is(err, store.ErrDuplicate):
			// A redelivery, or the same logical event twice. Already recorded,
			// so this is success.
			r.log.DebugContext(ctx, "activity already recorded",
				slog.String("dedupeKey", a.DedupeKey))
		default:
			errs = append(errs, err)
			// Do not queue an event for something that failed to store.
			continue
		}

		if err := r.enqueue(ctx, a); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (r *Recorder) enqueue(ctx context.Context, a *store.Activity) error {
	event, err := publisher.ActivityEvent(a)
	if err != nil {
		return err
	}
	return r.store.Enqueue(ctx, []*store.Outbound{event})
}
