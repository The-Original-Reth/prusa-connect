package app

// What the fleet is printing, and what it printed last.
//
// Everything here is read out of the state tree the drivers already fill, and
// nothing here is a second channel from a driver. A print reaches this server as
// ordinary state patches at /state, /job/id, /job/path, /job/progress and
// /job/time_remaining, so a driver that reports a job needs no method of its
// own and a driver that reports none costs nothing.
//
// The row's readings are derived from the state tree; which row they belong to
// is not. A print is named by a UUID, and the flush matches each reading to the
// print the device began last (printidentity.go). That memory is loaded from
// printer_job at start, so a print survives a restart rather than a session: a
// machine still printing when it comes back is matched to the row that was
// already there.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prusa3d/prusa-connect/internal/authz"
	"github.com/prusa3d/prusa-connect/internal/platform/devicestore"
	"github.com/prusa3d/prusa-connect/internal/transport/http/httpapi"
	"github.com/prusa3d/prusa-connect/internal/transport/http/httpfault"
	"github.com/prusa3d/prusa-connect/sdk/connectorapi"
	"github.com/prusa3d/prusa-connect/sdk/fault"
)

// Where a print is addressed in the state tree. The paths are the driver
// contract's own, so what a driver is told to report and what this reads cannot
// drift apart.
const (
	printerStatePath   = "/state"
	jobStatePath       = connectorapi.JobStatePath
	jobIDPath          = connectorapi.JobIDPath
	jobProgressPath    = connectorapi.JobProgressPath
	jobRemainingPath   = connectorapi.JobTimeRemainingPath
	jobPathPath        = connectorapi.JobPathPath
	jobDisplayNamePath = connectorapi.JobDisplayPath
	jobPrintingPath    = connectorapi.JobTimePrintingPath
	jobOutcomePath     = connectorapi.JobOutcomePath
	jobFilamentPath    = connectorapi.JobFilamentUsedPath
)

// millisecondsPerSecond converts the machine's own estimate, which is whole
// seconds, into the milliseconds the HTTP surface publishes.
const millisecondsPerSecond = 1000

// The states a machine reports while it is holding a print. They are the
// simulator's vocabulary (internal/printer/simulator/commands.go) and the
// xBuddy dialect's, transcribed rather than invented: this server has no word
// for a printer state that the driver did not give it.
//
// ATTENTION is in the set and is the one that needs saying. A machine with a
// dialog on the screen reports ATTENTION instead of PRINTING, and the print is
// still on the bed: a rule that read it as the end of the job would close the
// row every time the firmware asked a question and reopen it when the operator
// answered.
//
// The whole /state vocabulary is IDLE, PRINTING, PAUSED, FINISHED, STOPPED,
// READY, ERROR, BUSY, ATTENTION and UNKNOWN, which is to_str(DeviceState) in
// Prusa-Firmware-Buddy/src/state/printer_state.cpp:563. The four words no set
// here names are the four that say nothing about a print: IDLE and READY are a
// free bed, UNKNOWN is a machine that will not say, and BUSY is a long move,
// held for 80 to 100 seconds by an MK4 homing and levelling its bed.
// ERROR is likewise unnamed, because a machine in it has stopped and the
// firmware, not this server, decides what that did to the print.
const (
	statePrinting  = "PRINTING"
	statePaused    = "PAUSED"
	stateAttention = "ATTENTION"
	stateStopped   = "STOPPED"
	stateFinished  = "FINISHED"
	stateError     = "ERROR"
	stateBusy      = "BUSY"
)

// jobRunningStates is what keeps a print current. Every other state ends it,
// which is deliberately the open half of the rule: FINISHED and STOPPED are the
// two a machine means it by, and IDLE, READY or a state this build has never
// heard of, reported over a job that has not ended, is a machine saying the
// print is off the bed in the only words it has.
//
//nolint:gochecknoglobals // a fixed set, read-only after init.
var jobRunningStates = []string{statePrinting, statePaused, stateAttention}

// machineOverrulesTheJob are the machine states that are worth more than what
// the print last said about itself.
//
// They guard against a stale JOB_INFO. /job/state comes out of a JOB_INFO
// frame and /state comes off every telemetry frame, so the two can disagree
// for as long as the printer goes unasked. A print that is paused and then
// stopped can report /state PAUSED and then STOPPED while /job/state says
// PRINTING throughout, and the row would keep PRINTING for a print that was
// aborted. The driver asks for a fresh JOB_INFO on every such move. This set
// decides what the record says while the answer is in flight, or on a
// firmware that never answers.
//
// The vocabularies are the same words, so this is a choice between two readings
// and not a translation: /state carries the ten words the firmware spells at
// Prusa-Firmware-Buddy/src/state/printer_state.cpp:563, and /job/state is one
// of PRINTING, FINISHED and IDLE. PAUSED, STOPPED and FINISHED are the three
// that overrule, because each of them is the machine saying something about the
// print that a print still claiming to run cannot be right about.
//
// BUSY is deliberately not here, and that is the same decision the driver
// takes for /job/state: a long move is the machine talking about itself, so it
// overrules nothing the print said. It cannot end the print either; see
// jobFacts.ended.
//
// IDLE is deliberately not here, and neither is ATTENTION. A machine goes back
// to IDLE after a print ends and the print's own last word is how it ended, so
// reading IDLE over FINISHED would overwrite the answer with the absence of
// one; ATTENTION is a dialog on the screen over a print that is still on the
// bed.
//
// IDLE still overrules a print that claims to run, which is a different case:
// the machine says the print is over (jobFacts.ended) and the driver has not
// yet written how it ended. A flush in that gap freezes the machine's word
// instead of a PRINTING that no later reading takes back.
//
// PRINTING is here for the same reason PAUSED is, from the other side. A print
// that says it is paused on a machine that says it is printing is the same
// stale JOB_INFO: the xBuddy firmware renders that frame's state as PRINTING
// only while its own device state is exactly Printing and as PAUSED for every
// other one, so an answer that arrived while a dialog stood says PAUSED about a
// print nobody paused, and the machine's PRINTING is the newer and truer of the
// two. It cannot overrule a print that has ended, because only a
// print still claiming to run is overruled at all.
//
//nolint:gochecknoglobals // a fixed set, read-only after init.
var machineOverrulesTheJob = []string{statePrinting, statePaused, stateStopped, stateFinished}

// currentAndLastJobs is how many rows the jobs read asks for. The current job
// is the newest row when it has not ended, and the last job is the newest row
// that has, so two rows answer both questions on a machine that is printing and
// on one that is not.
const currentAndLastJobs = 2

// jobFacts is what one state tree says about the print on that machine. It is
// comparable on purpose: the flush holds the last one per entry and compares
// the whole struct, so a print whose progress has not moved costs no write.
type jobFacts struct {
	// printerState is the machine's own word for what it is doing, from /state.
	// It is what decides whether the print is still on the bed, because it is
	// the one a driver refreshes on every frame.
	printerState string
	// jobState is the machine's word for what the print is doing, from
	// /job/state and from /state when the driver reports no separate one.
	//
	// The two differ after a print ends, and that is why both are here: the
	// machine moves on to IDLE or READY, and the print's last state stays
	// FINISHED or STOPPED. Storing the machine's would overwrite how the print
	// ended with what the machine did next.
	jobState string
	// machineID is the job number the machine issued, as it spelled it. It is
	// kept as text rather than parsed: the number is the machine's, this server
	// only names a row with it, and a firmware that numbers jobs with something
	// other than an integer is then still tracked.
	machineID   string
	path        string
	displayName string
	progress    float64
	remaining   int64
	// outcome is the driver's own reading of how the print ended, from
	// /job/outcome, and is empty for a driver that reports none. The set of
	// words is connectorapi's and the mapping onto it belongs to the driver:
	// this server knows no machine's vocabulary, so a print that ends with
	// nothing here ends as unknown.
	outcome string
	// printing is how long the machine says it has printed, in seconds, and
	// filament is how much filament it says the print used, in millimetres.
	// Each has a flag beside it because a machine that reports none and a
	// machine that reported zero are different answers, and only the flag
	// separates them.
	printing      int64
	printingKnown bool
	filament      float64
	filamentKnown bool
	// objects is the bounded set of objects the machine can cancel one by
	// one, as the document the row keeps, and empty for a machine that said
	// nothing about them. See jobobjects.go.
	objects string
}

// jobFactsOf reads one state tree.
func jobFactsOf(state map[string]json.RawMessage) jobFacts {
	printing, printingKnown := jobCount(state[jobPrintingPath])
	filament, filamentKnown := jobCount(state[jobFilamentPath])

	facts := jobFacts{
		printerState:  jobText(state[printerStatePath]),
		jobState:      jobStateOf(state),
		machineID:     jobText(state[jobIDPath]),
		path:          jobText(state[jobPathPath]),
		displayName:   jobDisplayNameOf(state),
		progress:      jobNumber(state[jobProgressPath]),
		remaining:     int64(jobNumber(state[jobRemainingPath])),
		outcome:       jobText(state[jobOutcomePath]),
		printing:      int64(printing),
		printingKnown: printingKnown,
		filament:      filament,
		filamentKnown: filamentKnown,
		objects:       jobObjectsOf(state),
	}

	if facts.reachedTheEnd() {
		facts.progress = wholePrint
		facts.remaining = 0
	}

	return facts
}

// wholePrint is the progress of a print that reached the end of its file.
const wholePrint = 100

// reachedTheEnd reports whether the print ended because the machine reached
// the end of the file.
//
// A machine sends no progress once it says FINISHED, so the last number it sent
// is the one from a second before the end, and a short print ends on 99. A
// print that was stopped or failed keeps the figure it stopped at, and a row is
// written once when it ends, so a wrong 100 would stay for good.
//
// The driver's outcome decides when there is one. xbuddy derives it from the
// same terminal word in the same frame, first word wins, so it never arrives
// after a FINISHED that it contradicts. A driver that reports no outcome is
// read by its words alone: the print's FINISHED counts only while the machine
// itself does not say STOPPED or ERROR.
func (f jobFacts) reachedTheEnd() bool {
	if !f.ended() {
		return false
	}

	if f.outcome != "" {
		return f.outcome == connectorapi.JobFinished
	}

	return f.jobState == stateFinished &&
		f.printerState != stateStopped && f.printerState != stateError
}

// jobStateOf is what the print says it is doing, which is the machine's own
// state on a driver that reports no separate one and on a driver whose separate
// one has gone stale.
//
// The staleness rule is the second one and is machineOverrulesTheJob's: a print
// that still claims to be running, on a machine that says it is paused, stopped
// or finished, is a JOB_INFO nobody has refreshed. The machine's word is the
// newer of the two by construction, because it arrives on every telemetry frame
// and the other arrives only when the printer is asked.
//
// It runs both ways round. A machine that is printing under a /job/state which
// says the print is over is the same stale JOB_INFO seen from the other side:
// Buddy's /job/state is one of PRINTING, FINISHED and IDLE and it moves at the
// start of a print and on a reconnect, so a host that reattached between two
// prints holds IDLE over a machine that is printing. Reading the print's word
// there would answer IDLE for a job the machine says is running.
func jobStateOf(state map[string]json.RawMessage) string {
	machine := jobText(state[printerStatePath])

	reported := jobText(state[jobStatePath])
	if reported == "" {
		return machine
	}

	if slices.Contains(jobRunningStates, reported) &&
		(slices.Contains(machineOverrulesTheJob, machine) || (jobFacts{printerState: machine}).ended()) {
		return machine
	}

	if !slices.Contains(jobRunningStates, reported) && slices.Contains(jobRunningStates, machine) {
		return machine
	}

	return reported
}

// jobDisplayNameOf is what to show the print as.
//
// It is the name the firmware gave, and the file's own base name where the
// firmware gave none. Buddy's JOB_INFO carries a long name only for a file whose
// name it shortened. Without the fallback, a print of a plainly named file
// would reach the history with an empty display_name, and a client would have
// nothing to render. The base name is part of the path the machine did report.
func jobDisplayNameOf(state map[string]json.RawMessage) string {
	if reported := jobText(state[jobDisplayNamePath]); reported != "" {
		return reported
	}

	printed := jobText(state[jobPathPath])
	if printed == "" {
		return ""
	}

	return path.Base(printed)
}

// held reports whether the machine has said anything about a print at all. A
// tree with no job id has never carried one, and a device that has never
// printed gets no row.
func (f jobFacts) held() bool { return f.machineID != "" }

// running reports whether the print is still on the bed.
//
// BUSY is the second clause and it is the same fact as in ended: a machine
// reports BUSY while a long move runs and says nothing about a print by it. So
// the print's own word decides under one, and a print that still says it is
// running is still running. Reading the machine's word alone would make a long
// move mid-print a print that is neither running nor ended. The row would stay
// open and current_job_id would be cleared for the length of the move, so the
// listing would say the device prints nothing while its own row says the print
// has not finished.
func (f jobFacts) running() bool {
	if slices.Contains(jobRunningStates, f.printerState) {
		return true
	}

	return f.printerState == stateBusy && slices.Contains(jobRunningStates, f.jobState)
}

// underWay reports whether the machine says a print is moving on its bed,
// which is running less ATTENTION.
//
// It is what a reading under the number of a print that ended has to say to be
// a print of its own. ATTENTION alone is not evidence of a print: the xBuddy
// firmware raises it for a print-preview dialog before any print starts, such
// as the file error after a stopped transfer, and names the previewed file
// under the number of the print before it, because its counter moves only when
// a print starts. Over a print that is open, ATTENTION is still a
// dialog over that print, and running is the rule there.
func (f jobFacts) underWay() bool { return f.running() && f.printerState != stateAttention }

// ended reports whether this reading is the machine saying the print is over,
// which is what stamps a row's end.
//
// It is not the negation of running, and BUSY is the difference. A machine
// reports BUSY while a long move runs and says nothing about a print by it, so
// a BUSY arriving over a job that has not ended must not close the row: an end
// stamped once is the row's for ever, and closing a running print on a homing
// move would put a wrong finish time on it. This holds whether or not a
// firmware reports BUSY over a print.
//
// running names BUSY as well, and the two are still not each other's
// negation: a long move over a print that has already ended is neither running
// nor an end, because the end was stamped when the print ended and the row
// keeps the first one it was given.
//
// A machine that has said nothing about its state has not said the print
// ended, which is the first clause. The state tree is filled one path at a
// time, and a flush reads it between two of them: a reading that put the job
// number in the tree before the machine's own state would otherwise stamp an
// end on a print that had just begun, and an end stamped once is the row's for
// ever.
func (f jobFacts) ended() bool {
	return f.printerState != "" && !f.running() && f.printerState != stateBusy
}

// nullNode is what a driver writes to say a path is gone. A tree this server
// holds should carry none, because record drops the path rather than storing
// the word; it is checked anyway, because a tree restored from a store another
// build wrote is not this process's own.
const nullNode = "null"

// jobText reads a state node as text. A driver reports a state as a JSON string
// and a job id as a number, and both are named here, so a value that is neither
// is rendered as the machine encoded it rather than dropped: what this reads is
// an identifier, and a firmware spelling one in a way this build did not expect
// still identifies a print.
func jobText(value json.RawMessage) string {
	if len(value) == 0 || string(value) == nullNode {
		return ""
	}

	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		return text
	}

	return strings.TrimSpace(string(value))
}

// jobNumber reads a state node as a number, and reads anything else as zero. A
// progress this server cannot parse is not a reason to lose the job.
func jobNumber(value json.RawMessage) float64 {
	if len(value) == 0 {
		return 0
	}

	number, err := strconv.ParseFloat(strings.TrimSpace(string(value)), 64)
	if err != nil {
		return 0
	}

	return number
}

// jobCount reads a state node as a number and reports whether the node carried
// one. A path the machine never wrote and a path it wrote zero to are different
// answers, and the caller publishes the first as absent.
func jobCount(value json.RawMessage) (float64, bool) {
	if len(value) == 0 || string(value) == nullNode {
		return 0, false
	}

	number, err := strconv.ParseFloat(strings.TrimSpace(string(value)), 64)
	if err != nil {
		return 0, false
	}

	return number, true
}

// jobRowOf is the printer_job row one written record stands for, and reports
// false for a device that has never printed.
//
// The two timestamps are the record's last_seen rather than the wall clock,
// because that is when the machine said what this row holds. A print whose
// start is read off a restored record then keeps the moment the machine was
// last heard from, and not the moment this process happened to come back.
//
// started is only ever the first write's, and ended only ever the first write
// that carries one: both are held there by the upsert, not by this. So a row
// derived again from a restored tree restates the readings and moves neither.
func jobRowOf(record devicestore.DeviceRecord, facts jobFacts, printID string) (devicestore.PrinterJob, bool) {
	if !facts.held() || printID == "" {
		return devicestore.PrinterJob{}, false
	}

	job := devicestore.PrinterJob{
		JobID:         printID,
		DeviceID:      record.DeviceID,
		DeviceJobID:   facts.machineID,
		Path:          facts.path,
		DisplayName:   facts.displayName,
		State:         facts.jobState,
		Progress:      facts.progress,
		TimeRemaining: facts.remaining,
		Started:       record.LastSeen,
		Objects:       facts.objects,
	}

	// The outcome rides on the write that ends the print, as the settled frame
	// does, so the row and the frame say the same thing about how it ended.
	if facts.ended() {
		job.Ended = record.LastSeen
		job.Outcome = jobOutcomeOf(facts.outcome)
	}

	return job, true
}

// writeJobs stores the prints that moved in this flush.
//
// It runs after the record write and before the record frames, in the same
// flush and on the same goroutine. After, because a job names a device and the
// device's row is what the foreign key is onto; before the frames, because a
// subscriber told the record moved may read the job the record's
// current_job_id names.
//
// A failure is logged and not retried. A live device restates its whole job
// on the next tick, and re-marking would let
// one unwritable row hold every other machine's print out of the store for as
// long as the process runs.
func (d *devices) writeJobs(ctx context.Context, changed []changedRecord) {
	rows := make([]devicestore.PrinterJob, 0, len(changed))
	facts := make(map[string]jobFacts, len(changed))

	for _, taken := range changed {
		if !taken.jobMoved {
			continue
		}

		if row, held := jobRowOf(taken.record, taken.job, taken.printID); held {
			rows = append(rows, withOdometer(row, taken.record.State, taken.job))
			facts[row.DeviceID] = taken.job
		}
	}

	if len(rows) == 0 {
		return
	}

	// Asked before the write, because an upsert leaves a row it created and a
	// row it restated reading the same afterwards.
	owed := d.jobEventsOwed(ctx, rows, facts)

	if err := d.records.SaveDeviceJobs(ctx, rows...); err != nil {
		d.log.ErrorContext(ctx, "the prints in progress could not be written to the record store",
			"devices", len(rows), "error", err.Error())

		return
	}

	d.publishJobEvents(ctx, owed)
}

// Jobs answers what one device is printing and what it printed before that.
//
// It is authorized exactly as the device read is, through the same reach check
// and the same action: a caller who reaches nothing on the machine and an id
// this server has never been dialled by get the one refusal, so nothing here
// tells anybody a printer exists. A job is a reading about a machine, so it
// takes observe and not operate.
//
// It reads the store rather than the live entry, and that is the whole point of
// the table. The entry holds the print that is running; the store holds the one
// before it, and holds both across a restart.
func (d *devices) Jobs(ctx context.Context, principal string, id string) (httpapi.DeviceJobs, error) {
	if _, _, err := d.reachHolding(ctx, principal, id, authz.ActionObserve); err != nil {
		return httpapi.DeviceJobs{}, err
	}

	stored, err := d.records.DeviceJobs(ctx, id, currentAndLastJobs)
	if err != nil {
		return httpapi.DeviceJobs{}, err
	}

	var out httpapi.DeviceJobs

	for _, job := range stored {
		rendered := d.renderJob(ctx, job)

		if job.Running() {
			if out.Current == nil {
				out.Current = &rendered
			}

			continue
		}

		if out.Last == nil {
			out.Last = &rendered
		}
	}

	return out, nil
}

// Job answers one print by its id.
//
// A print on a device the caller cannot observe is refused with the error an
// unknown id gets, so nothing here tells anybody a print exists. A store that
// cannot be read is the one other answer.
func (d *devices) Job(ctx context.Context, principal string, jobID string) (httpapi.DeviceJob, error) {
	stored, held, err := d.records.PrinterJobByID(ctx, jobID)
	if err != nil {
		return httpapi.DeviceJob{}, err
	}

	if !held {
		return httpapi.DeviceJob{}, jobNotFound(jobID)
	}

	if _, _, err = d.reachHolding(ctx, principal, stored.DeviceID, authz.ActionObserve); err != nil {
		if refused, is := errors.AsType[*fault.Fault](err); is &&
			(refused.Status() == http.StatusNotFound || refused.Status() == http.StatusForbidden) {
			return httpapi.DeviceJob{}, jobNotFound(jobID)
		}

		return httpapi.DeviceJob{}, err
	}

	return d.renderJob(ctx, stored), nil
}

// jobNotFound is the one refusal of a read by print id.
func jobNotFound(jobID string) error {
	return fault.PrinterJobNotFound.New("this server holds no print with that id that you may see").
		With(fault.StringParam("job_id", jobID))
}

// jobRender is one stored job on the wire. The store keeps the machine's own
// whole seconds and the wire takes milliseconds, so the estimate is converted
// here and nowhere else.
func jobRender(job devicestore.PrinterJob) httpapi.DeviceJob {
	out := httpapi.DeviceJob{
		ID:              job.JobID,
		DeviceJobID:     job.DeviceJobID,
		DeviceID:        job.DeviceID,
		Path:            job.Path,
		DisplayName:     job.DisplayName,
		State:           job.State,
		Progress:        job.Progress,
		TimeRemainingMS: job.TimeRemaining * millisecondsPerSecond,
		Started:         job.Started,
	}

	if !job.Ended.IsZero() {
		ended := job.Ended
		out.Ended = &ended
		// A row whose end was written without an outcome holds none, and reads
		// as a print whose machine said nothing about how it ended.
		out.Outcome = jobOutcomeOf(job.Outcome)
	}

	out.ObjectCount, out.Objects = renderJobObjects(job.Objects)

	if used, measured := job.FilamentUsed(); measured {
		out.FilamentUsedMM = &used
	}

	return out
}

// jobOf is the print on one device as the listing carries it, and is nil for a
// machine that has never printed.
//
// It is read off the state tree the entry already holds rather than out of the
// store, because a listing renders a page of devices and a read per row would
// be a statement per row.
//
// That is also why it carries no start and no end. The tree holds one reading
// and not the row's history, so a start invented here would be the moment the
// reading was taken and would disagree with the row a client then fetched. The
// route at /jobs is where the two timestamps come from, and it reads the table
// that has them.
func jobOf(state map[string]json.RawMessage, prints *printBook, id string) *httpapi.DeviceJobSummary {
	facts := jobFactsOf(state)
	if !facts.held() {
		return nil
	}

	summary := &httpapi.DeviceJobSummary{
		ID:              prints.idOf(id, facts),
		DeviceJobID:     facts.machineID,
		Path:            facts.path,
		DisplayName:     facts.displayName,
		State:           facts.jobState,
		Progress:        facts.progress,
		TimeRemainingMS: facts.remaining * millisecondsPerSecond,
		Running:         facts.running(),
	}

	summary.ObjectCount, summary.Objects = renderJobObjects(facts.objects)

	return summary
}

// The bounds on one page of a device's finished prints. They are the record
// store's own: a page is read out of one statement and rendered in the process,
// so the cap is what stops a caller asking for a machine's whole printing life
// in one response.
const (
	defaultJobHistoryPage = 50
	maxJobHistoryPage     = 200
)

// JobHistory returns one page of the prints that have ended, newest first, on
// one device or on every device the caller may observe.
//
// It takes observe, which is what the jobs read takes: a print is a reading
// about a machine and not something a caller does to one. A device the caller
// reaches nothing on and one this server has never been dialled by are the same
// refusal, so nothing here tells anybody a device exists.
//
// An empty id is every device the caller may observe. There is no device to
// refuse then, and the permission is the query's own: the statement joins the
// projected rows of this caller, so a print of a machine they reach nothing on
// never leaves the engine.
//
// It reads one row more than the page asks for, and answers a cursor only when
// that row came back. So the last page is the page that carries no cursor,
// rather than a client having to make one more request to find out.
func (d *devices) JobHistory(
	ctx context.Context, principal string, id string, query httpapi.DeviceJobQuery,
) (httpapi.DeviceJobPage, error) {
	if id != "" {
		if _, _, err := d.reachHolding(ctx, principal, id, authz.ActionObserve); err != nil {
			return httpapi.DeviceJobPage{}, err
		}
	}

	who, err := authz.ParseSubject(principal)
	if err != nil {
		return httpapi.DeviceJobPage{}, fmt.Errorf("read the caller's principal: %w", err)
	}

	started, after, err := parseJobCursor(query.Cursor)
	if err != nil {
		return httpapi.DeviceJobPage{}, err
	}

	limit := query.Limit
	if limit <= 0 {
		limit = defaultJobHistoryPage
	}

	limit = min(limit, maxJobHistoryPage)

	stored, err := d.records.EndedJobs(ctx, who, devicestore.JobWindow{
		DeviceID:     id,
		From:         query.From,
		To:           query.To,
		AfterStarted: started,
		AfterID:      after,
		Limit:        limit + 1,
	})
	if err != nil {
		return httpapi.DeviceJobPage{}, err
	}

	page := httpapi.DeviceJobPage{Items: make([]httpapi.DeviceJob, 0, limit), Limit: limit}

	for index, job := range stored {
		if index == limit {
			page.Cursor = d.cursorAfter(ctx, id, stored[limit-1])

			break
		}

		page.Items = append(page.Items, d.renderJob(ctx, job))
	}

	return page, nil
}

// ListJobs returns one page of the prints on every device the caller may
// observe, running and ended, newest first.
//
// There is no device to refuse, even when the query names one: the statement
// joins the caller's projected rows, so a device the caller reaches nothing on
// answers the empty page a device that never printed answers. A refusal there
// would tell a caller which ids exist.
func (d *devices) ListJobs(
	ctx context.Context, principal string, query httpapi.JobQuery,
) (httpapi.JobPage, error) {
	who, err := authz.ParseSubject(principal)
	if err != nil {
		return httpapi.JobPage{}, fmt.Errorf("read the caller's principal: %w", err)
	}

	started, after, err := parseJobCursor(query.Cursor)
	if err != nil {
		return httpapi.JobPage{}, err
	}

	limit := query.Limit
	if limit <= 0 {
		limit = defaultJobHistoryPage
	}

	limit = min(limit, maxJobHistoryPage)

	stored, err := d.fleetJobPage(ctx, who, query, started, after, limit+1)
	if err != nil {
		return httpapi.JobPage{}, err
	}

	page := httpapi.JobPage{Items: make([]httpapi.DeviceJob, 0, limit), Limit: limit}

	for index, job := range stored {
		if index == limit {
			page.Cursor = d.cursorAfter(ctx, query.DeviceID, stored[limit-1])

			break
		}

		page.Items = append(page.Items, d.renderJob(ctx, job))
	}

	return page, nil
}

// fleetJobPage reads one page of ListJobs. A range of ends is the page of
// ended prints the device history reads, over the whole fleet: it is ordered
// and resumed as the fleet's page is, so one cursor serves both.
func (d *devices) fleetJobPage(
	ctx context.Context, who authz.Subject, query httpapi.JobQuery,
	started time.Time, after string, limit int,
) ([]devicestore.PrinterJob, error) {
	if query.From.IsZero() && query.To.IsZero() {
		return d.records.FleetJobs(ctx, who, devicestore.JobFilter{
			DeviceID:     query.DeviceID,
			Phase:        devicestore.JobPhase(query.Phase),
			AfterStarted: started,
			AfterID:      after,
			Limit:        limit,
		})
	}

	return d.records.EndedJobs(ctx, who, devicestore.JobWindow{
		DeviceID:     query.DeviceID,
		From:         query.From,
		To:           query.To,
		AfterStarted: started,
		AfterID:      after,
		Limit:        limit,
	})
}

// cursorAfter is the cursor the next page resumes from, and empty where this
// page has to be the last one.
//
// A printer with no clock reports a start before jobCursorFloor, and the read
// back of a cursor naming such an instant is refused. Handing one out would
// give a client a cursor its next request cannot use, so the page ends here
// instead. The line is at debug because it is a fact about one printer's clock
// and an operator reading it can do nothing from a server.
func (d *devices) cursorAfter(ctx context.Context, device string, last devicestore.PrinterJob) string {
	if last.Started.Unix() < jobCursorFloor {
		d.log.DebugContext(ctx, "a page of the print history ends early because a print has no usable start",
			"device_id", device, "job_id", last.JobID,
			"started", last.Started.UTC().Format(time.RFC3339))

		return ""
	}

	return renderJobCursor(last.Started, last.JobID)
}

// renderJobCursor is where one page stopped, as text a caller sends back.
//
// It is encoded rather than written plainly so that nothing reads a job id or
// an instant out of it and starts depending on the form. What it holds is the
// order key of the last row: the instant the print started and the id, which is
// the tie-break the statement orders by.
//
// The instant is kept to the nanosecond, which is the whole value the record
// store answered with. A rounded one names an earlier instant than the row it
// came from, and the statement then steps over every print that started between
// the two.
func renderJobCursor(started time.Time, jobID string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(started.UTC().UnixNano(), 10) + jobCursorSeparator + jobID))
}

// jobCursorFloor is the earliest instant a cursor may name, in seconds since
// the Unix epoch, and it is 2020-01-01.
//
// A cursor names when a print started, and no print this server holds started
// before that. A number written in another unit decodes to an instant near
// 1970, and the statement would then answer no rows and no cursor, which a
// client reads as the end of the history. Refusing it says what happened.
const jobCursorFloor = 1577836800

// jobCursorSeparator divides the two halves. It is a byte no job id carries: an
// id is a UUID this server mints.
const jobCursorSeparator = "\x00"

// parseJobCursor reads a cursor back. A cursor this server did not mint is
// refused rather than read as the first page, because a client that mistyped
// one would otherwise be answered with a page it has already seen.
func parseJobCursor(cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", invalidJobCursor()
	}

	at, id, found := strings.Cut(string(decoded), jobCursorSeparator)
	if !found {
		return time.Time{}, "", invalidJobCursor()
	}

	nanos, err := strconv.ParseInt(at, 10, 64)
	if err != nil || nanos < jobCursorFloor*int64(time.Second) {
		return time.Time{}, "", invalidJobCursor()
	}

	return time.Unix(0, nanos).UTC(), id, nil
}

// invalidJobCursor is the one refusal this page has. It says what a cursor is
// rather than what was wrong with this one, because the form is not part of the
// contract and a caller cannot act on a complaint about it.
func invalidJobCursor() error {
	return httpfault.RequestInvalid("query.cursor",
		"the cursor a previous page answered with",
		"this server did not mint it")
}
