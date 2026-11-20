package httpapi

// The prints a device is running and has run. They answer from a table of
// their own. The device record holds one reading of the print on the bed. This
// table holds the row that reading belongs to, with the start and the end the
// reading cannot carry.

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/prusa3d/prusa-connect/internal/transport/http/httpfault"
)

// DeviceJobSummary is the print on a device as the device record carries it.
//
// It has no start and no end. It is read out of the state tree the machine
// last reported, which holds one reading and not the row's history. A
// timestamp here would be the moment the reading was taken, and it would
// disagree with the job record. Read `GET /api/v1/devices/{device_id}/jobs`
// for the row.
type DeviceJobSummary struct {
	ID          string `json:"id"            doc:"The print's identity, a version 7 UUID this server gives each print. It is the same id the jobs route reports. It is empty for a print the machine began since this server last wrote its records, which takes up to two seconds."`
	DeviceJobID string `json:"device_job_id" doc:"The job number the machine issued, as the machine spells it. It is not unique: a machine reuses its numbers after a firmware reset and when its counter wraps."`
	Path        string `json:"path"          doc:"The file on the machine's own storage, as the machine spells it. It is empty until the machine has said what it is printing."`
	DisplayName string `json:"display_name"  doc:"What the machine shows on its screen for the file, which is not always the last segment of the path."`

	// State is the machine's own word and carries no enum. The states belong to
	// the device kind that declares them, so a peer running a newer adapter
	// may name one this build does not know.
	State string `json:"state" doc:"What the machine says the print is doing, in the machine's own vocabulary. Read it as an open value: the states belong to the device kind that declares them, and the set is published with the kind."`

	Progress        float64 `json:"progress"          doc:"How far the print has got, in whole percent as the machine reports it."`
	TimeRemainingMS int64   `json:"time_remaining_ms" doc:"How much time the machine thinks is left, in milliseconds. The machine reports whole seconds, so this value is always a multiple of 1000. It is the machine's estimate and not this server's."`

	// Running is what a client branches on, so that it does not have to hold
	// its own list of which states mean the print is still on the bed.
	Running bool `json:"running" doc:"Whether the print is still on the bed. It is false once the machine reports a state that ends a job, and a client should read this rather than compare the state itself."`

	ObjectCount *int        `json:"object_count,omitempty" doc:"How many objects of the print the machine reported that can be cancelled one by one. It is absent when the machine said nothing about them, and 0 for a print that has none."`
	Objects     []JobObject `json:"objects,omitempty"      doc:"The objects of the print that can be cancelled one by one, ordered by id. It holds at most 256. When the machine reported more, every cancelled object is kept before the others, so an object missing from the list was not cancelled unless more than 256 were." maxItems:"256"`
}

// JobObject is one object of a print that the machine can skip while it
// prints the rest.
type JobObject struct {
	ID        int    `json:"id"             doc:"The machine's own number for the object. It is the id the core.command.cancel_object and core.command.uncancel_object verbs take."`
	Name      string `json:"name,omitempty" doc:"The label the machine gives the object, at most 64 bytes. It is absent when the machine reports no names."                         maxLength:"64"`
	Cancelled bool   `json:"cancelled"      doc:"Whether the machine skips the object for the rest of the print."`
}

// DeviceJob is one print as the record store holds it: what was printed, the
// last reading taken while it ran, and when it ran.
type DeviceJob struct {
	ID          string `json:"id"            doc:"The print's identity, a version 7 UUID this server gives each print. A print that reuses a job number the machine issued before is a print of its own with an id of its own."`
	DeviceJobID string `json:"device_job_id" doc:"The job number the machine issued, as the machine spells it. It is not unique: a machine reuses its numbers after a firmware reset and when its counter wraps."`
	DeviceID    string `json:"device_id"     doc:"The device the print ran on, as the listing reports it."`
	Path        string `json:"path"          doc:"The file on the machine's own storage, as the machine spells it."`
	DisplayName string `json:"display_name"  doc:"What the machine shows on its screen for the file."`

	State string `json:"state" doc:"The machine's own word for what the print was doing when this row was last written. Read it as an open value: the states belong to the device kind that declares them."`

	Progress        float64 `json:"progress"          doc:"How far the print had got when this row was last written, in whole percent. A print that finished reads 100 and one that was interrupted keeps the figure it was interrupted at."`
	TimeRemainingMS int64   `json:"time_remaining_ms" doc:"How much time the machine thought was left when this row was last written, in milliseconds. The machine reports whole seconds, so this value is always a multiple of 1000."`

	Started time.Time  `json:"started"           doc:"When this server first saw the print."`
	Ended   *time.Time `json:"ended,omitempty"   doc:"When this server saw the print stop. It is absent while the print is running, which is what tells the current job from a finished one."`
	Outcome string     `json:"outcome,omitempty" doc:"How the print ended, as the machine's driver reported it, and what the device.job.settled event says. It is absent while the print is running. A print whose machine said nothing about how it ended reads unknown, and so does one that ended before this server kept the outcome." enum:"finished,failed,cancelled,unknown"`

	ObjectCount *int        `json:"object_count,omitempty" doc:"How many objects of the print the machine reported that can be cancelled one by one, as of the last reading this row holds. It is absent when the machine said nothing about them, and 0 for a print that has none. A finished print keeps the count it ended with."`
	Objects     []JobObject `json:"objects,omitempty"      doc:"The objects of the print that can be cancelled one by one, ordered by id, as of the last reading this row holds. A finished print keeps the objects it ended with, which says which of them were cancelled. It holds at most 256. When the machine reported more, every cancelled object is kept before the others, so an object missing from the list was not cancelled unless more than 256 were." maxItems:"256"`

	//nolint:lll // the huma tag is one field's whole contract and does not wrap.
	FilamentUsedMM *float64 `json:"filament_used_mm,omitempty" doc:"The filament the print used, in millimetres, measured by the machine's extruder odometer: the reading when the print began taken off the last reading while it ran. It counts so far while the print runs. It is absent when the machine reports no odometer, for a print this server first saw after it began, and for a print stored before this server kept the odometer."`
	//nolint:lll // the huma tag is one field's whole contract and does not wrap.
	Slicer *JobSlicer `json:"slicer,omitempty" doc:"What the slicer said about the whole print, read from the file when this server sent it to the machine. It is absent for a file the machine got another way, because a machine tells this server nothing about what is inside its files."`
}

// JobSlicer is what the slicer wrote into a print file about the whole print.
type JobSlicer struct {
	FilamentMM       float64 `json:"filament_mm,omitempty"       doc:"The filament the whole print uses, in millimetres. Absent when the slicer did not say."`
	FilamentG        float64 `json:"filament_g,omitempty"        doc:"The filament the whole print uses, in grams. Absent when the slicer did not say."`
	EstimatedSeconds int64   `json:"estimated_seconds,omitempty" doc:"How long the slicer said the print takes in its normal mode, in seconds. Absent when the slicer did not say."`
	Material         string  `json:"material,omitempty"          doc:"The filament type the file was sliced for, such as PLA. Absent when the slicer did not say."`
}

// DeviceJobs is what one device is printing and what it printed before that.
//
// Both members are optional, and all four combinations occur. A machine that
// has never printed carries neither. One printing its first job carries only
// the current. One that has stopped carries only the last. One printing again
// carries both.
type DeviceJobs struct {
	Current *DeviceJob `json:"current,omitempty" doc:"The print on the bed now. It is absent when the machine is not printing."`
	Last    *DeviceJob `json:"last,omitempty"    doc:"The most recent print that has ended. It is absent until one has, and it is not the same row as the current print ever is."`
}

type getDeviceJobsInput struct {
	DeviceRef
}

type deviceJobsOutput struct {
	Body DeviceJobs
}

func registerDeviceJobs(api huma.API, fleet DeviceFleet) {
	register(api, huma.Operation{
		OperationID: "get-device-jobs",
		Method:      http.MethodGet,
		Path:        deviceJobsPath,
		Summary:     "The print on a device and the one before it",
		Description: "What the machine is printing now and what it printed last, from the record " +
			"store rather than from the live session. So a print survives a restart of this server: " +
			"the row was written as the machine reported it and is read back here whether or not " +
			"the machine is connected." + paragraphBreak +
			"`current` is the print on the bed and is absent when there is none. `last` is the most " +
			"recent print that ended, and the two are never the same row. A print's `state` is the " +
			"machine's own word for what it is doing, from the vocabulary the device kind declares, " +
			"so read it as an open value." + paragraphBreak +
			"It takes observe on the device, which is what `GET /api/v1/devices/{device_id}` takes: " +
			"a job is a reading about a machine. A device the caller reaches nothing on and an id " +
			"this server has never been dialled by are the same 404.",
		Tags:   []string{"devices"},
		Errors: deviceErrors,
	}, func(ctx context.Context, in *getDeviceJobsInput) (*deviceJobsOutput, error) {
		found, err := fleet.Jobs(ctx, principalFrom(ctx), in.DeviceID)
		if err != nil {
			return nil, err
		}

		return &deviceJobsOutput{Body: found}, nil
	})
}

// jobPath is one print, by the id the jobs route and the history report.
const jobPath = "/api/v1/jobs/{job_id}"

type getJobInput struct {
	JobID string `path:"job_id" doc:"The print's identity, as the jobs route and the print history report it." pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$" example:"0199e3a4-5bb5-7aac-8a2c-9c3982e6373f"`
}

type jobOutput struct {
	Body DeviceJob
}

func registerJob(api huma.API, fleet DeviceFleet) {
	register(api, huma.Operation{
		OperationID: "get-job",
		Method:      http.MethodGet,
		Path:        jobPath,
		Summary:     "One print, by its id",
		Description: "The print a link names, whichever device it ran on and whether it is running " +
			"or has ended. The id is the one `GET /api/v1/devices/{device_id}/jobs` and the print " +
			"history report, and a print keeps it for good. The upgrade that brought these ids deleted " +
			"the prints stored before it, so an older id the history still holds names no print." +
			paragraphBreak +
			"It takes observe on the device the print ran on, which is what the jobs read takes. A " +
			"print on a device the caller reaches nothing on and an id this server never gave are " +
			"the same 404, so asking for ids does not tell anybody which prints exist.",
		Tags:   []string{"devices"},
		Errors: []int{http.StatusNotFound},
	}, func(ctx context.Context, in *getJobInput) (*jobOutput, error) {
		found, err := fleet.Job(ctx, principalFrom(ctx), in.JobID)
		if err != nil {
			return nil, err
		}

		return &jobOutput{Body: found}, nil
	})
}

// DeviceJobQuery is one request for a page of a device's finished prints: the
// range of time it covers, how many rows at a time, and where the last page
// ended.
type DeviceJobQuery struct {
	// From and To bound when the print ended, and the zero value of either is
	// no bound on that end. They bound the end and not the start, because a
	// report over a month asks what finished in that month.
	From time.Time
	To   time.Time

	// Limit is how many rows to answer with. Zero is the server's default page
	// size, and a value above the cap is answered with the cap.
	Limit int

	// Cursor is what the previous page reported. It is minted by this server
	// and its form is not part of the contract.
	Cursor string
}

// DeviceJobPage is one page of the prints that have ended on a device.
//
// There is no total. Counting the rows to render a pager would scan the whole
// table on every page. The cursor says whether there is more.
type DeviceJobPage struct {
	//nolint:lll // the huma tag is one field's whole contract and does not wrap.
	Items []DeviceJob `json:"items" doc:"The prints that have ended on this device, newest first. A print that is still running is not here; read the device record for that one."`
	//nolint:lll // the huma tag is one field's whole contract and does not wrap.
	Cursor string `json:"cursor" doc:"Opaque text to send back as the cursor parameter for the next page. Empty when this page is the last one. Read nothing out of its form."`
	//nolint:lll // the huma tag is one field's whole contract and does not wrap.
	Limit int `json:"limit" doc:"The page size the server applied, which is the cap when a larger one was asked for."`
}

type listDeviceJobsInput struct {
	DeviceRef

	//nolint:lll // the huma tag is one field's whole contract and does not wrap.
	From time.Time `query:"from" doc:"Only prints that ended at or after this instant, as RFC 3339. Omitted is no lower bound." example:"2026-09-01T00:00:00Z"`

	//nolint:lll // the huma tag is one field's whole contract and does not wrap.
	To time.Time `query:"to" doc:"Only prints that ended at or before this instant, as RFC 3339. Omitted is no upper bound." example:"2026-10-01T00:00:00Z"`

	//nolint:lll // the huma tag is one field's whole contract and does not wrap.
	Limit int `query:"limit" minimum:"0" doc:"How many prints to return. Omitted or zero is the server's default page size, and a value above the cap is answered with the cap." example:"50"`

	//nolint:lll // the huma tag is one field's whole contract and does not wrap.
	Cursor string `query:"cursor" maxLength:"128" doc:"The cursor a previous page returned. It is minted by this server and its form is not part of this contract." example:"MTc1NjE5OTY0NTAwMDo5ZjJhNGMxOC02ZDNiLTRlMDctYjVhMS04YzBmMmQ3ZTRiOTM6NDE="`
}

type listDeviceJobsOutput struct {
	Body DeviceJobPage
}

// validJobRange refuses a range that can hold nothing. A from after a to is a
// caller's mistake, and answering it with an empty page would look like a
// device that printed nothing.
func validJobRange(from, to time.Time) error {
	if from.IsZero() || to.IsZero() || !from.After(to) {
		return nil
	}

	return httpfault.RequestInvalid("query.from", "an instant at or before to",
		"the range starts after it ends, so it holds no print")
}

func registerDeviceJobHistory(api huma.API, fleet DeviceFleet) {
	register(api, huma.Operation{
		OperationID: "list-device-jobs",
		Method:      http.MethodGet,
		Path:        deviceJobHistory,
		Summary:     "The prints that have ended on a device",
		Description: "A page of the prints this device has finished, newest first. " +
			"`GET /api/v1/devices/{device_id}/jobs` answers the print on the bed and the one before " +
			"it, and this answers the rest: what a device page shows under a heading a person " +
			"scrolls." + paragraphBreak +
			"Only prints that have ended are here. The print on the bed has no end yet, so it is " +
			"the jobs read's `current` and never a row on this page, and a client that shows both " +
			"reads the two operations once each." + paragraphBreak +
			"`from` and `to` narrow the page to the prints that ended inside a range of time, " +
			"which is what a report over a month reads. Either one alone bounds that end only, " +
			"and a `from` after a `to` is refused." + paragraphBreak +
			"Paging follows the `cursor` member: send it back as the `cursor` parameter for the " +
			"next page, and stop when it comes back empty. The rows are read from the record " +
			"store, so they survive a restart of this server and a device whose session has ended " +
			"answers exactly as a connected one does." + paragraphBreak +
			"It takes observe on the device, which is what the jobs read takes: a print is a " +
			"reading about a machine. A device the caller reaches nothing on and an id this server " +
			"has never been dialled by are the same 404.",
		Tags:   []string{"devices"},
		Errors: append([]int{http.StatusBadRequest}, deviceErrors...),
	}, func(ctx context.Context, in *listDeviceJobsInput) (*listDeviceJobsOutput, error) {
		if err := validJobRange(in.From, in.To); err != nil {
			return nil, err
		}

		page, err := fleet.JobHistory(ctx, principalFrom(ctx), in.DeviceID,
			DeviceJobQuery{From: in.From, To: in.To, Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, err
		}

		return &listDeviceJobsOutput{Body: page}, nil
	})
}
