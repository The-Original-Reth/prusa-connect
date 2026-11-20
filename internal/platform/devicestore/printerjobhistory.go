package devicestore

// The page of finished prints, over one device or over every device a subject
// may observe. It is one statement, because the permission is resolved inside
// it: a page that fetched rows and then dropped the ones the caller may not see
// would leak through its own paging even when every row it returned was right.

import (
	"context"
	"time"

	"github.com/prusa3d/prusa-connect/internal/authz"
)

// selectEndedJobsSQL is the page. Every predicate is unconditional and is
// switched by the value it is given, so one statement serves a page over one
// device and a page over a fleet, with or without a time range, on the first
// page and on the ones after it.
//
// The join resolves the permission. device_access is internal/authz's
// projected row per subject and device. It is the one table of another owner
// that a statement here reads, and authz.DeviceAccessKey is the key it is read
// by. A print of a device the subject reaches nothing on never leaves the
// engine.
//
// The comparisons that resume a page are written out and not as a row value,
// because SQLite compiles a row-value comparison only from 3.15 and this
// string has to render for both engines.
const selectEndedJobsSQL = `SELECT j.printer_job_id, j.device_id, j.device_job_id, j.path, j.display_name, j.state,
		j.progress, j.time_remaining, j.started, j.ended, j.objects, j.outcome,
		j.odometer_start, j.odometer_end, j.odometer_id
	FROM printer_job j
	JOIN device_access a ON a.device_id = j.device_id
	WHERE a.subject_type = ? AND a.subject_id = ? AND (a.actions & ?) <> 0
		AND j.ended IS NOT NULL
		AND (? = '' OR j.device_id = ?)
		AND j.ended >= ? AND j.ended <= ?
		AND (j.started < ? OR (j.started = ? AND j.printer_job_id < ?))
	ORDER BY j.started DESC, j.printer_job_id DESC
	LIMIT ?`

// endOfTime stands in for "no bound" on the upper end of a range and for "the
// first page" on the cursor. It is a real instant and not a missing predicate,
// so one statement serves a bounded query and an unbounded one, and neither
// engine has to compare against a null.
var endOfTime = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)

// JobWindow is one request for a page of finished prints.
//
// DeviceID empty is every device the subject may observe. From and To bound
// when the print ended, and the zero value of either is no bound on that end.
// AfterStarted and AfterID are where the previous page stopped, and a zero
// AfterStarted asks for the first page.
//
// Limit is how many rows the page carries, and a Limit above maxPageLimit reads
// maxPageLimit rows.
type JobWindow struct {
	DeviceID string

	From time.Time
	To   time.Time

	AfterStarted time.Time
	AfterID      string

	Limit int
}

// EndedJobs reads a page of the prints that have ended, newest first, at most
// Limit of them.
//
// The permission is resolved in the query, against observe on each device. A
// print is a reading about a machine and not something a caller does to one.
// A device the subject reaches nothing on contributes no row and no count.
//
// The cursor is the start instant and the job id of the last row, and not an
// offset. A print that ends while somebody is paging would move every later
// row down by one, and a page walked by offset would then repeat one row and
// skip another.
func (s *Store) EndedJobs(
	ctx context.Context, subject authz.Subject, window JobWindow,
) ([]PrinterJob, error) {
	if window.Limit <= 0 {
		return nil, nil
	}

	subjectType, subjectID, mask, err := authz.DeviceAccessKey(ctx, subject, authz.ActionObserve)
	if err != nil {
		return nil, err
	}

	// The page size this statement runs with, and the size of the slice it fills.
	// A caller asking for more gets maxPageLimit rows, so the number in the
	// request never decides how much memory this server reserves.
	limit := min(window.Limit, maxPageLimit)

	from := window.From.UTC()

	to := window.To.UTC()
	if window.To.IsZero() {
		to = endOfTime
	}

	after := window.AfterStarted.UTC()
	if window.AfterStarted.IsZero() {
		after = endOfTime
	}

	rows, err := s.db.QueryContext(ctx, s.q(selectEndedJobsSQL),
		subjectType, subjectID, mask,
		window.DeviceID, window.DeviceID,
		from, to,
		after, after, window.AfterID,
		limit,
	)
	if err != nil {
		return nil, failure("read the finished printer jobs", err)
	}

	defer func() { _ = rows.Close() }() // the scan error below is the one worth reporting

	out := make([]PrinterJob, 0, limit)

	for rows.Next() {
		job, scanErr := scanPrinterJob(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		out = append(out, job)
	}

	if err = rows.Err(); err != nil {
		return nil, failure("read the finished printer jobs", err)
	}

	return out, nil
}
