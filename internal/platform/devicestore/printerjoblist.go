package devicestore

// The page of prints across the fleet, running and ended, over every device a
// subject may observe. The permission is resolved inside the statement for the
// reason printerjobhistory.go gives: a page filtered after the read would leak
// through its own paging.

import (
	"context"
	"fmt"
	"time"

	"github.com/prusa3d/prusa-connect/internal/authz"
)

// JobPhase narrows a page of prints to the running ones or the ended ones. The
// zero value asks for both.
type JobPhase string

// The phases a page can be narrowed to. A print is running while its row has no
// end.
const (
	JobPhaseAny     JobPhase = ""
	JobPhaseRunning JobPhase = "running"
	JobPhaseEnded   JobPhase = "ended"
)

// The page is one of six statements, one per pair of filters, so an index
// serves each one and the walk stops at the limit. One statement that switched
// each filter on a parameter would keep the device and the phase off every
// index, and SQLite would sort every print the caller may see for each page.
//
// device_access is internal/authz's projected row per subject and device,
// read through an EXISTS so each row costs one lookup.
const (
	fleetJobsSelect = `SELECT j.printer_job_id, j.device_id, j.device_job_id, j.path, j.display_name, j.state,
		j.progress, j.time_remaining, j.started, j.ended, j.objects, j.outcome,
		j.odometer_start, j.odometer_end, j.odometer_id
	FROM printer_job j
	WHERE `

	fleetJobsAfter = `(j.started < ? OR (j.started = ? AND j.printer_job_id < ?))`

	fleetJobsObserved = `
		AND EXISTS (SELECT 1 FROM device_access a
			WHERE a.device_id = j.device_id
				AND a.subject_type = ? AND a.subject_id = ? AND (a.actions & ?) <> 0)
	ORDER BY j.started DESC, j.printer_job_id DESC
	LIMIT ?`

	fleetJobsOnDevice = `j.device_id = ? AND `
	fleetJobsRunning  = ` AND j.ended IS NULL`
	fleetJobsEnded    = ` AND j.ended IS NOT NULL`
)

var fleetJobsStatements = map[bool]map[JobPhase]string{
	false: {
		JobPhaseAny:     fleetJobsSelect + fleetJobsAfter + fleetJobsObserved,
		JobPhaseRunning: fleetJobsSelect + fleetJobsAfter + fleetJobsRunning + fleetJobsObserved,
		JobPhaseEnded:   fleetJobsSelect + fleetJobsAfter + fleetJobsEnded + fleetJobsObserved,
	},
	true: {
		JobPhaseAny:     fleetJobsSelect + fleetJobsOnDevice + fleetJobsAfter + fleetJobsObserved,
		JobPhaseRunning: fleetJobsSelect + fleetJobsOnDevice + fleetJobsAfter + fleetJobsRunning + fleetJobsObserved,
		JobPhaseEnded:   fleetJobsSelect + fleetJobsOnDevice + fleetJobsAfter + fleetJobsEnded + fleetJobsObserved,
	},
}

// fleetJobsStatement is the statement one filter reads with. A phase outside
// the three would match no row and read as a fleet that printed nothing, so it
// is refused.
func fleetJobsStatement(filter JobFilter) (string, error) {
	statement, ok := fleetJobsStatements[filter.DeviceID != ""][filter.Phase]
	if !ok {
		return "", fmt.Errorf("devicestore: %q is not a job phase", string(filter.Phase))
	}

	return statement, nil
}

// JobFilter is one request for a page of prints across the fleet.
//
// DeviceID empty is every device the subject may observe, and a device the
// subject may not observe contributes no row whether or not it is named.
// AfterStarted and AfterID are where the previous page stopped, and a zero
// AfterStarted asks for the first page. A Limit above maxPageLimit reads
// maxPageLimit rows.
type JobFilter struct {
	DeviceID string
	Phase    JobPhase

	AfterStarted time.Time
	AfterID      string

	Limit int
}

// FleetJobs reads a page of prints, running and ended, newest start first, at
// most Limit of them, against observe on each device.
func (s *Store) FleetJobs(ctx context.Context, subject authz.Subject, filter JobFilter) ([]PrinterJob, error) {
	statement, err := fleetJobsStatement(filter)
	if err != nil {
		return nil, err
	}

	if filter.Limit <= 0 {
		return nil, nil
	}

	subjectType, subjectID, mask, err := authz.DeviceAccessKey(ctx, subject, authz.ActionObserve)
	if err != nil {
		return nil, err
	}

	limit := min(filter.Limit, maxPageLimit)

	after := filter.AfterStarted.UTC()
	if filter.AfterStarted.IsZero() {
		after = endOfTime
	}

	args := make([]any, 0, 8) //nolint:mnd // the widest statement's parameters
	if filter.DeviceID != "" {
		args = append(args, filter.DeviceID)
	}

	args = append(args, after, after, filter.AfterID, subjectType, subjectID, mask, limit)

	rows, err := s.db.QueryContext(ctx, s.q(statement), args...)
	if err != nil {
		return nil, failure("read the printer jobs across the fleet", err)
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
		return nil, failure("read the printer jobs across the fleet", err)
	}

	return out, nil
}
