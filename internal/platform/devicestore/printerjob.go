package devicestore

// The print a machine is running, and the ones it ran before. printer_job
// carries a foreign key onto authz_device, so a job can never name a machine
// the directory does not hold, and removing a device removes its prints.
//
// This is not a history provider. A provider stores every sample a machine
// reported and answers questions over a window. This stores one row per print
// and answers two questions about one machine: what is it printing, and what
// did it print last. A fleet screen asks both on every page load, and
// answering them from a sample store would be a scan.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	// A job write is not a grant write, for the reason a record write is not:
	// nothing it touches decides who may see what, so it bumps no epoch and
	// recompiles no projection.
	//
	// started is written once. The conflict clause keeps the first value,
	// because a running print is upserted on every flush that moves it. A start
	// time that moved with those writes would be the time of the last reading
	// and not of the print.
	//
	// ended is written once too, through COALESCE: the first write that carries
	// an end wins. A restart depends on it. A process that comes back holding a
	// restored state tree matches it to the same print and writes it again.
	// Without the COALESCE that write would move the end of a print that
	// finished before the shutdown to the moment the process started.
	upsertPrinterJobPrefix = `INSERT INTO printer_job
		(printer_job_id, device_id, device_job_id, path, display_name, state,
		 progress, time_remaining, started, ended, objects, outcome,
		 odometer_start, odometer_end, odometer_id)
		VALUES `

	// A print that has ended is finished, and every column of it is frozen at
	// the reading that ended it.
	//
	// Holding `ended` alone is not enough. The row is derived from the device's
	// state tree on every flush that moved it, and a tree can restate an older
	// reading. A restart reads one back out of the record store, and the
	// machine reuses its job numbers. Without the guard, a late write would
	// rewrite a print that stopped at 17 per cent as PRINTING at 8 per cent.
	//
	// The guard is on `ended` and not on the state word, because the set of
	// terminal words belongs to the machine's dialect and this table must not
	// hold a copy of it. A row with an end is done. A row without one takes
	// every reading it is given.
	//
	// The odometer columns follow the same guard and two rules of their own.
	// The start is taken once, from the first write that carries one, with the
	// odometer's id beside it. A write carries a start only for a print that
	// has just begun (internal/app's odometerStartOf), so a print first seen
	// part way has none. The end is the newest reading under the start's id
	// that is not below the start: a machine that resets its odometers reports
	// readings under a new id, and those are not this print's.
	upsertPrinterJobSuffix = `
		ON CONFLICT (printer_job_id) DO UPDATE SET
			path = CASE WHEN printer_job.ended IS NULL THEN excluded.path
				ELSE printer_job.path END,
			display_name = CASE WHEN printer_job.ended IS NULL THEN excluded.display_name
				ELSE printer_job.display_name END,
			state = CASE WHEN printer_job.ended IS NULL THEN excluded.state
				ELSE printer_job.state END,
			progress = CASE WHEN printer_job.ended IS NULL THEN excluded.progress
				ELSE printer_job.progress END,
			time_remaining = CASE WHEN printer_job.ended IS NULL THEN excluded.time_remaining
				ELSE printer_job.time_remaining END,
			objects = CASE WHEN printer_job.ended IS NULL THEN excluded.objects
				ELSE printer_job.objects END,
			outcome = CASE WHEN printer_job.ended IS NULL THEN excluded.outcome
				ELSE printer_job.outcome END,
			odometer_start = CASE WHEN printer_job.ended IS NULL AND printer_job.odometer_start IS NULL
				THEN excluded.odometer_start ELSE printer_job.odometer_start END,
			odometer_id = CASE WHEN printer_job.ended IS NULL AND printer_job.odometer_start IS NULL
				THEN excluded.odometer_id ELSE printer_job.odometer_id END,
			odometer_end = CASE
				WHEN printer_job.ended IS NOT NULL THEN printer_job.odometer_end
				WHEN excluded.odometer_end IS NULL THEN printer_job.odometer_end
				WHEN printer_job.odometer_start IS NULL THEN excluded.odometer_end
				WHEN excluded.odometer_id = printer_job.odometer_id
					AND excluded.odometer_end >= printer_job.odometer_start THEN excluded.odometer_end
				ELSE printer_job.odometer_end END,
			ended = COALESCE(printer_job.ended, excluded.ended)`

	// Newest first, which is the order both readers want: the current job is
	// the first row when it has no end, and the last job is the first row that
	// has one. The index on (device_id, started) makes it a lookup and not a
	// scan of every print in the fleet.
	selectPrinterJobsSQL = `SELECT ` + printerJobColumns + `
		FROM printer_job
		WHERE device_id = ?
		ORDER BY started DESC, printer_job_id DESC
		LIMIT ?`

	// The newest print of every device, in the order selectPrinterJobsSQL reads
	// one device's: a row is the latest when no row of the same device started
	// after it, or started with it under a greater id.
	selectLatestPrinterJobsSQL = `SELECT ` + printerJobColumns + `
		FROM printer_job j
		WHERE NOT EXISTS (
			SELECT 1 FROM printer_job k
			WHERE k.device_id = j.device_id
				AND (k.started > j.started
					OR (k.started = j.started AND k.printer_job_id > j.printer_job_id)))`

	selectPrinterJobByIDSQL = `SELECT ` + printerJobColumns + `
		FROM printer_job
		WHERE printer_job_id = ?`

	// An end written on its own, for a print the machine replaced without
	// saying how it ended. The first end stays, as it does in the upsert, and
	// so does the outcome written with it.
	endPrinterJobSQL = `UPDATE printer_job
		SET outcome = CASE WHEN ended IS NULL THEN ? ELSE outcome END,
			ended = COALESCE(ended, ?)
		WHERE printer_job_id = ?`

	// printerJobColumns is the column list every read selects, in the order
	// scanPrinterJob reads it.
	printerJobColumns = `printer_job_id, device_id, device_job_id, path, display_name, state,
			progress, time_remaining, started, ended, objects, outcome,
			odometer_start, odometer_end, odometer_id`
)

const (
	// jobColumns is how many parameters one job costs.
	jobColumns = 15

	// jobBatchRows is how many jobs one statement carries. 66 rows of 15
	// columns is 990 parameters, inside the 999 SQLite shipped before 3.32.
	jobBatchRows = 66
)

// ErrPrinterJobUnknownDevice reports a job written for a device the directory
// does not hold. The foreign key refuses it, as it does for
// ErrDeviceRecordUnknownDevice.
var ErrPrinterJobUnknownDevice = errors.New("devicestore: no device is registered under that id")

// PrinterJob is one print: what was printed, what the machine made of it, and
// when it ran.
//
// A job has no identity half, unlike a device record. Every column but the id
// and the device is the last reading taken while the print ran, and the row
// stops changing when the print ends.
type PrinterJob struct {
	// JobID is the print's identity, a version 7 UUID the record flush mints
	// once per print.
	JobID string

	// DeviceID is the machine that ran it.
	DeviceID string

	// DeviceJobID is the job number the machine issued, as text. It is not
	// unique: a machine reuses its numbers after a reset and after the counter
	// wraps.
	DeviceJobID string

	// Path is the file on the machine's own storage, and DisplayName is what
	// the firmware shows for it.
	Path        string
	DisplayName string

	// State is the printer's word for what the job is doing, from the
	// vocabulary the driver reports. Nothing here holds it to a set: the states
	// belong to the device kind that declares them.
	State string

	// Progress is whole percent as the machine reports it, and TimeRemaining is
	// the seconds it thinks are left. Both are the last reading taken, so a
	// print that was interrupted keeps the figures it was interrupted at.
	Progress      float64
	TimeRemaining int64

	// Started is when this server first saw the print, and Ended is when it saw
	// it stop. Ended is the zero time while the print is running, which is the
	// null in the column: a job with no end is the current one.
	Started time.Time
	Ended   time.Time

	// Objects is the objects of the print that can be cancelled one by one,
	// as a JSON document the host wrote, and empty for a print whose machine
	// said nothing about them. This store keeps the text and reads nothing
	// in it; internal/app owns its shape.
	Objects string

	// Outcome is how the print ended, in the word internal/app wrote with the
	// end, and empty while it runs. It is taken only by the write that ends the
	// print, and kept from then on like every other column. A row whose end
	// was written without one holds none. This store holds the word and knows
	// no set of them.
	Outcome string

	// OdometerStart is the extruder's odometer, in millimetres, when the print
	// began, and OdometerID is the odometer's id then. OdometerEnd is the
	// newest reading under that id while the print ran. Each is nil when the
	// machine reported none. OdometerStart is nil for a print first seen after
	// it began, because a reading then would count less than the print used.
	OdometerStart *float64
	OdometerEnd   *float64
	OdometerID    *int64
}

// FilamentUsed is the filament the print used as the machine's odometer
// measured it, in millimetres. It reports false when that cannot be measured:
// no odometer reading at the start, or none since.
func (j PrinterJob) FilamentUsed() (float64, bool) {
	if j.OdometerStart == nil || j.OdometerEnd == nil || *j.OdometerEnd < *j.OdometerStart {
		return 0, false
	}

	return *j.OdometerEnd - *j.OdometerStart, true
}

// Running reports whether this print has not ended.
func (j PrinterJob) Running() bool { return j.Ended.IsZero() }

// SaveDeviceJobs writes one row per job, creating it or overwriting the
// readings on it, in one transaction and in as few statements as the parameter
// limit allows.
//
// It is called from the record flush, because a job changes when the device's
// state tree does. A timer of its own would be a second transaction on the
// same tick. Writing nothing is not an error and opens no transaction.
func (s *Store) SaveDeviceJobs(ctx context.Context, jobs ...PrinterJob) error {
	if len(jobs) == 0 {
		return nil
	}

	// One row per job, last write winning. ON CONFLICT DO UPDATE refuses to
	// touch one row twice inside a single statement on Postgres, and a caller
	// that has coalesced by device already holds one job each, so this guards
	// against the caller that has not.
	deduplicated, err := deduplicateJobs(jobs)
	if err != nil {
		return err
	}

	return saveBatched(ctx, s, upsertBatch[PrinterJob]{
		what:         "printer job",
		rows:         deduplicated,
		prefix:       upsertPrinterJobPrefix,
		suffix:       upsertPrinterJobSuffix,
		columns:      jobColumns,
		perStatement: jobBatchRows,
		args:         jobArgs,
		classify:     s.jobWriteFailure,
	})
}

// deduplicateJobs returns one entry per job in the order the jobs first
// appeared, carrying the last one given for each.
func deduplicateJobs(jobs []PrinterJob) ([]PrinterJob, error) {
	at := make(map[string]int, len(jobs))
	out := make([]PrinterJob, 0, len(jobs))

	for _, job := range jobs {
		if job.JobID == "" {
			return nil, errors.New("devicestore: a printer job has no id")
		}

		if job.DeviceID == "" {
			return nil, errors.New("devicestore: a printer job names no device")
		}

		if index, seen := at[job.JobID]; seen {
			out[index] = job

			continue
		}

		at[job.JobID] = len(out)
		out = append(out, job)
	}

	return out, nil
}

// jobArgs is one batch flattened into the parameter list its VALUES rows take,
// in column order.
func jobArgs(batch []PrinterJob) []any {
	out := make([]any, 0, len(batch)*jobColumns)

	for _, job := range batch {
		out = append(out,
			job.JobID, job.DeviceID, job.DeviceJobID, job.Path, job.DisplayName, job.State,
			job.Progress, job.TimeRemaining, job.Started.UTC(), endedValue(job.Ended), job.Objects,
			outcomeValue(job), nullable(job.OdometerStart), nullable(job.OdometerEnd), nullable(job.OdometerID))
	}

	return out
}

// outcomeValue is the outcome as the column takes it: the word for a print
// this write ends, and nothing for one still running, so a reading that
// restates a running print cannot store an outcome before the end.
func outcomeValue(job PrinterJob) string {
	if job.Ended.IsZero() {
		return ""
	}

	return job.Outcome
}

// nullable is a pointer as a column takes it: NULL for nil.
func nullable[T any](value *T) any {
	if value == nil {
		return nil
	}

	return *value
}

// endedValue is the end of a print as the column takes it. A running print
// stores NULL and not the zero time. A zero timestamp is a real instant, and a
// reader filtering on it would have to know which of the two spellings this
// build used.
func endedValue(ended time.Time) any {
	if ended.IsZero() {
		return nil
	}

	return ended.UTC()
}

// DeviceJobs reads the newest jobs on one device, newest first, at most limit
// of them.
//
// It reads this table and joins nothing. Whether the caller may see the device
// is decided before this is reached, by the reach check the device read makes.
// This method does not answer the authorization question a second time.
func (s *Store) DeviceJobs(ctx context.Context, deviceID string, limit int) ([]PrinterJob, error) {
	if limit <= 0 {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, s.q(selectPrinterJobsSQL), deviceID, limit)
	if err != nil {
		return nil, failure("read the printer jobs", err)
	}

	defer func() { _ = rows.Close() }() // the scan error below is the one worth reporting

	var out []PrinterJob

	for rows.Next() {
		job, scanErr := scanPrinterJob(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		out = append(out, job)
	}

	if err = rows.Err(); err != nil {
		return nil, failure("read the printer jobs", err)
	}

	return out, nil
}

// LatestDeviceJobs reads the newest print of every device, keyed by device id.
// A device that has never printed is not in the map.
//
// The record flush loads it at start, so the first reading of each machine is
// matched against the print it ran last without a read per device.
func (s *Store) LatestDeviceJobs(ctx context.Context) (map[string]PrinterJob, error) {
	rows, err := s.db.QueryContext(ctx, s.q(selectLatestPrinterJobsSQL))
	if err != nil {
		return nil, failure("read the latest printer jobs", err)
	}

	defer func() { _ = rows.Close() }() // the scan error below is the one worth reporting

	out := map[string]PrinterJob{}

	for rows.Next() {
		job, scanErr := scanPrinterJob(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		out[job.DeviceID] = job
	}

	if err = rows.Err(); err != nil {
		return nil, failure("read the latest printer jobs", err)
	}

	return out, nil
}

// PrinterJobByID reads one print by its id, and reports false for an id the
// table does not hold. Like DeviceJobs it decides nothing about who may see it.
func (s *Store) PrinterJobByID(ctx context.Context, jobID string) (PrinterJob, bool, error) {
	rows, err := s.db.QueryContext(ctx, s.q(selectPrinterJobByIDSQL), jobID)
	if err != nil {
		return PrinterJob{}, false, failure("read a printer job", err)
	}

	defer func() { _ = rows.Close() }() // the scan error below is the one worth reporting

	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return PrinterJob{}, false, failure("read a printer job", err)
		}

		return PrinterJob{}, false, nil
	}

	job, err := scanPrinterJob(rows)
	if err != nil {
		return PrinterJob{}, false, err
	}

	return job, true, nil
}

// EndPrinterJob stamps an end and its outcome on one print and changes nothing
// else. It is for a print the machine replaced with another one without saying
// how it ended, so the readings the row holds are the last ones the machine
// gave. An end already there is kept, and so is its outcome.
func (s *Store) EndPrinterJob(ctx context.Context, jobID string, ended time.Time, outcome string) error {
	if _, err := s.db.ExecContext(ctx, s.q(endPrinterJobSQL), outcome, ended.UTC(), jobID); err != nil {
		return failure("end a printer job", err)
	}

	return nil
}

// scanPrinterJob reads one row of the job query.
func scanPrinterJob(rows interface{ Scan(...any) error }) (PrinterJob, error) {
	var (
		job                        PrinterJob
		ended                      sql.NullTime
		odometerStart, odometerEnd sql.NullFloat64
		odometerID                 sql.NullInt64
	)

	err := rows.Scan(
		&job.JobID, &job.DeviceID, &job.DeviceJobID, &job.Path, &job.DisplayName, &job.State,
		&job.Progress, &job.TimeRemaining, &job.Started, &ended, &job.Objects, &job.Outcome,
		&odometerStart, &odometerEnd, &odometerID,
	)
	if err != nil {
		return PrinterJob{}, failure("scan a printer job", err)
	}

	job.Started = job.Started.UTC()

	if ended.Valid {
		job.Ended = ended.Time.UTC()
	}

	if odometerStart.Valid {
		job.OdometerStart = &odometerStart.Float64
	}

	if odometerEnd.Valid {
		job.OdometerEnd = &odometerEnd.Float64
	}

	if odometerID.Valid {
		job.OdometerID = &odometerID.Int64
	}

	return job, nil
}

// jobWriteFailure names the one refusal a caller can act on, the way
// recordWriteFailure does. The directory is asked about every device in the
// refused batch. The driver's error text is not matched, because the two
// engines spell a foreign key violation differently and the statement carried
// up to jobBatchRows jobs.
func (s *Store) jobWriteFailure(ctx context.Context, batch []PrinterJob, cause error) error {
	var unregistered []string

	for _, job := range batch {
		var held string

		err := s.db.QueryRowContext(ctx, s.q(selectDeviceSQL), job.DeviceID).Scan(&held)
		if errors.Is(err, sql.ErrNoRows) && !slices.Contains(unregistered, job.DeviceID) {
			unregistered = append(unregistered, job.DeviceID)
		}
	}

	failure := failure("write the printer jobs", cause)

	if len(unregistered) == 0 {
		return failure
	}

	return fmt.Errorf("%w: %s. Register the device before writing its jobs: authz_device is what "+
		"says which devices exist, and a job is state about a row in it: %w",
		ErrPrinterJobUnknownDevice, strings.Join(unregistered, ", "), failure)
}
