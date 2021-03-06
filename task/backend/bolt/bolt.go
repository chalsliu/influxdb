// Package bolt provides an bolt-backed store implementation.
//
// The data stored in bolt is structured as follows:
//
//    bucket(/tasks/v1/tasks) key(:task_id) -> Content of submitted task (i.e. flux code).
//    bucket(/tasks/v1/task_meta) key(:task_id) -> Protocol Buffer encoded backend.StoreTaskMeta,
//                                    so we have a consistent view of runs in progress and max concurrency.
//    bucket(/tasks/v1/org_by_task_id) key(task_id) -> The organization ID (stored as encoded string) associated with given task.
//    bucket(/tasks/v1/name_by_task_id) key(:task_id) -> The user-supplied name of the script.
//    bucket(/tasks/v1/run_ids) -> Counter for run IDs
//    bucket(/tasks/v1/orgs).bucket(:org_id) key(:task_id) -> Empty content; presence of :task_id allows for lookup from org to tasks.
// Note that task IDs are stored big-endian uint64s for sorting purposes,
// but presented to the users with leading 0-bytes stripped.
// Like other components of the system, IDs presented to users may be `0f12` rather than `f12`.
package bolt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	bolt "github.com/coreos/bbolt"
	platform "github.com/influxdata/influxdb"
	"github.com/influxdata/influxdb/snowflake"
	"github.com/influxdata/influxdb/task/backend"
	"github.com/influxdata/influxdb/task/options"
)

// ErrDBReadOnly is an error for when the database is set to read only.
// Tasks needs to be able to write to the db.
var ErrDBReadOnly = errors.New("db is read only")

// ErrMaxConcurrency is an error for when the max concurrency is already
// reached for a task when you try to schedule a task.
var ErrMaxConcurrency = errors.New("max concurrency reached")

// ErrRunNotFound is an error for when a run isn't found in a FinishRun method.
var ErrRunNotFound = errors.New("run not found")

// ErrNotFound is an error for when a task could not be found
var ErrNotFound = errors.New("task not found")

// Store is task store for bolt.
type Store struct {
	db     *bolt.DB
	bucket []byte
	idGen  platform.IDGenerator

	minLatestCompleted int64
}

const basePath = "/tasks/v1/"

var (
	tasksPath    = []byte(basePath + "tasks")
	orgsPath     = []byte(basePath + "orgs")
	taskMetaPath = []byte(basePath + "task_meta")
	orgByTaskID  = []byte(basePath + "org_by_task_id")
	nameByTaskID = []byte(basePath + "name_by_task_id")
	runIDs       = []byte(basePath + "run_ids")
)

// Option is a optional configuration for the store.
type Option func(*Store)

// NoCatchUp allows you to skip any task that was supposed to run during down time.
func NoCatchUp(st *Store) { st.minLatestCompleted = time.Now().Unix() }

// New gives us a new Store based on "github.com/coreos/bbolt"
func New(db *bolt.DB, rootBucket string, opts ...Option) (*Store, error) {
	if db.IsReadOnly() {
		return nil, ErrDBReadOnly
	}
	bucket := []byte(rootBucket)

	err := db.Update(func(tx *bolt.Tx) error {
		// create root
		root, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		// create the buckets inside the root
		for _, b := range [][]byte{
			tasksPath, orgsPath, taskMetaPath,
			orgByTaskID, nameByTaskID, runIDs,
		} {
			_, err := root.CreateBucketIfNotExists(b)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	st := &Store{db: db, bucket: bucket, idGen: snowflake.NewDefaultIDGenerator(), minLatestCompleted: math.MinInt64}
	for _, opt := range opts {
		opt(st)
	}
	return st, nil
}

// CreateTask creates a task in the boltdb task store.
func (s *Store) CreateTask(ctx context.Context, req backend.CreateTaskRequest) (platform.ID, error) {
	o, err := backend.StoreValidator.CreateArgs(req)
	if err != nil {
		return platform.InvalidID(), err
	}
	// Get ID
	id := s.idGen.ID()
	err = s.db.Update(func(tx *bolt.Tx) error {
		// get the root bucket
		b := tx.Bucket(s.bucket)
		name := []byte(o.Name)
		// Encode ID
		encodedID, err := id.Encode()
		if err != nil {
			return err
		}

		// write script
		err = b.Bucket(tasksPath).Put(encodedID, []byte(req.Script))
		if err != nil {
			return err
		}

		// name
		err = b.Bucket(nameByTaskID).Put(encodedID, name)
		if err != nil {
			return err
		}

		// Encode org ID
		encodedOrg, err := req.Org.Encode()
		if err != nil {
			return err
		}

		// org
		orgB, err := b.Bucket(orgsPath).CreateBucketIfNotExists(encodedOrg)
		if err != nil {
			return err
		}

		err = orgB.Put(encodedID, nil)
		if err != nil {
			return err
		}

		err = b.Bucket(orgByTaskID).Put(encodedID, encodedOrg)
		if err != nil {
			return err
		}

		stm := backend.NewStoreTaskMeta(req, o)
		stmBytes, err := stm.Marshal()
		if err != nil {
			return err
		}
		metaB := b.Bucket(taskMetaPath)
		return metaB.Put(encodedID, stmBytes)
	})

	if err != nil {
		return platform.InvalidID(), err
	}

	return id, nil
}

func (s *Store) UpdateTask(ctx context.Context, req backend.UpdateTaskRequest) (backend.UpdateTaskResult, error) {
	var res backend.UpdateTaskResult
	op, err := backend.StoreValidator.UpdateArgs(req)
	if err != nil {
		return res, err
	}

	encodedID, err := req.ID.Encode()
	if err != nil {
		return res, err
	}

	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		bt := b.Bucket(tasksPath)

		v := bt.Get(encodedID)
		if v == nil {
			return backend.ErrTaskNotFound
		}
		res.OldScript = string(v)
		if res.OldScript == "" {
			return errors.New("task script not stored properly")
		}
		var newScript string
		if !req.Options.IsZero() || req.Script != "" {
			if err = req.UpdateFlux(res.OldScript); err != nil {
				return err
			}
			newScript = req.Script
		}
		if req.Script == "" {
			// Need to build op from existing script.
			op, err = options.FromScript(res.OldScript)
			if err != nil {
				return err
			}
			newScript = res.OldScript
		} else {
			op, err = options.FromScript(req.Script)
			if err != nil {
				return err
			}
			if err := bt.Put(encodedID, []byte(req.Script)); err != nil {
				return err
			}
			if err := b.Bucket(nameByTaskID).Put(encodedID, []byte(op.Name)); err != nil {
				return err
			}
		}

		var orgID platform.ID

		if err := orgID.Decode(b.Bucket(orgByTaskID).Get(encodedID)); err != nil {
			return err
		}

		stmBytes := b.Bucket(taskMetaPath).Get(encodedID)
		if stmBytes == nil {
			return backend.ErrTaskNotFound
		}
		var stm backend.StoreTaskMeta
		if err := stm.Unmarshal(stmBytes); err != nil {
			return err
		}
		stm.UpdatedAt = time.Now().Unix()
		res.OldStatus = backend.TaskStatus(stm.Status)

		if req.Status != "" {
			stm.Status = string(req.Status)
		}
		if req.AuthorizationID.Valid() {
			stm.AuthorizationID = uint64(req.AuthorizationID)
		}
		stmBytes, err = stm.Marshal()
		if err != nil {
			return err
		}
		if err := b.Bucket(taskMetaPath).Put(encodedID, stmBytes); err != nil {
			return err
		}
		res.NewMeta = stm

		res.NewTask = backend.StoreTask{
			ID:     req.ID,
			Org:    orgID,
			Name:   op.Name,
			Script: newScript,
		}

		return nil
	})
	return res, err
}

// ListTasks lists the tasks based on a filter.
func (s *Store) ListTasks(ctx context.Context, params backend.TaskSearchParams) ([]backend.StoreTaskWithMeta, error) {
	if params.PageSize < 0 {
		return nil, errors.New("ListTasks: PageSize must be positive")
	}
	if params.PageSize > platform.TaskMaxPageSize {
		return nil, fmt.Errorf("ListTasks: PageSize exceeds maximum of %d", platform.TaskMaxPageSize)
	}
	lim := params.PageSize
	if lim == 0 {
		lim = platform.TaskDefaultPageSize
	}
	taskIDs := make([]platform.ID, 0, lim)
	var tasks []backend.StoreTaskWithMeta

	if err := s.db.View(func(tx *bolt.Tx) error {
		var c *bolt.Cursor
		b := tx.Bucket(s.bucket)
		if params.Org.Valid() {
			encodedOrg, err := params.Org.Encode()
			if err != nil {
				return err
			}
			orgB := b.Bucket(orgsPath).Bucket(encodedOrg)
			if orgB == nil {
				return ErrNotFound
			}
			c = orgB.Cursor()
		} else {
			c = b.Bucket(tasksPath).Cursor()
		}
		if params.After.Valid() {
			encodedAfter, err := params.After.Encode()
			if err != nil {
				return err
			}

			// If the taskID returned by c.Seek is greater than after param, append taskID to taskIDs.
			k, _ := c.Seek(encodedAfter)
			if bytes.Compare(k, encodedAfter) > 0 {
				var nID platform.ID
				if err := nID.Decode(k); err != nil {
					return err
				}
				taskIDs = append(taskIDs, nID)
			}

			for k, _ := c.Next(); k != nil && len(taskIDs) < lim; k, _ = c.Next() {
				var nID platform.ID
				if err := nID.Decode(k); err != nil {
					return err
				}
				taskIDs = append(taskIDs, nID)
			}
		} else {
			for k, _ := c.First(); k != nil && len(taskIDs) < lim; k, _ = c.Next() {
				var nID platform.ID
				if err := nID.Decode(k); err != nil {
					return err
				}
				taskIDs = append(taskIDs, nID)
			}
		}

		tasks = make([]backend.StoreTaskWithMeta, len(taskIDs))
		for i := range taskIDs {
			// TODO(docmerlin): optimization: don't check <-ctx.Done() every time though the loop
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				// TODO(docmerlin): change the setup to reduce the number of lookups to 1 or 2.
				encodedID, err := taskIDs[i].Encode()
				if err != nil {
					return err
				}
				tasks[i].Task.ID = taskIDs[i]
				tasks[i].Task.Script = string(b.Bucket(tasksPath).Get(encodedID))
				tasks[i].Task.Name = string(b.Bucket(nameByTaskID).Get(encodedID))
			}
		}
		if params.Org.Valid() {
			for i := range taskIDs {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					tasks[i].Task.Org = params.Org
				}
			}
			goto POPULATE_META
		}
		for i := range taskIDs {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				encodedID, err := taskIDs[i].Encode()
				if err != nil {
					return err
				}

				var orgID platform.ID
				if err := orgID.Decode(b.Bucket(orgByTaskID).Get(encodedID)); err != nil {
					return err
				}
				tasks[i].Task.Org = orgID
			}
		}

	POPULATE_META:
		for i := range taskIDs {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				encodedID, err := taskIDs[i].Encode()
				if err != nil {
					return err
				}

				var stm backend.StoreTaskMeta
				if err := stm.Unmarshal(b.Bucket(taskMetaPath).Get(encodedID)); err != nil {
					return err
				}

				if stm.LatestCompleted < s.minLatestCompleted {
					stm.LatestCompleted = s.minLatestCompleted
					stm.AlignLatestCompleted()
				}

				tasks[i].Meta = stm
			}
		}
		return nil
	}); err != nil {
		if err == ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return tasks, nil
}

// FindTaskByID finds a task with a given an ID.  It will return nil if the task does not exist.
func (s *Store) FindTaskByID(ctx context.Context, id platform.ID) (*backend.StoreTask, error) {
	var orgID platform.ID
	var script, name string
	encodedID, err := id.Encode()
	if err != nil {
		return nil, err
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		scriptBytes := b.Bucket(tasksPath).Get(encodedID)
		if scriptBytes == nil {
			return backend.ErrTaskNotFound
		}
		script = string(scriptBytes)

		if err := orgID.Decode(b.Bucket(orgByTaskID).Get(encodedID)); err != nil {
			return err
		}

		name = string(b.Bucket(nameByTaskID).Get(encodedID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &backend.StoreTask{
		ID:     id,
		Org:    orgID,
		Name:   name,
		Script: script,
	}, err
}

func (s *Store) FindTaskMetaByID(ctx context.Context, id platform.ID) (*backend.StoreTaskMeta, error) {
	var stm backend.StoreTaskMeta
	encodedID, err := id.Encode()
	if err != nil {
		return nil, err
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		stmBytes := b.Bucket(taskMetaPath).Get(encodedID)
		if stmBytes == nil {
			return backend.ErrTaskNotFound
		}
		return stm.Unmarshal(stmBytes)
	})
	if err != nil {
		return nil, err
	}

	if stm.LatestCompleted < s.minLatestCompleted {
		stm.LatestCompleted = s.minLatestCompleted
		stm.AlignLatestCompleted()
	}

	return &stm, nil
}

func (s *Store) FindTaskByIDWithMeta(ctx context.Context, id platform.ID) (*backend.StoreTask, *backend.StoreTaskMeta, error) {
	var stmBytes []byte
	var orgID platform.ID
	var script, name string
	encodedID, err := id.Encode()
	if err != nil {
		return nil, nil, err
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		scriptBytes := b.Bucket(tasksPath).Get(encodedID)
		if scriptBytes == nil {
			return backend.ErrTaskNotFound
		}
		script = string(scriptBytes)

		// Assign copies of everything so we don't hold a stale reference to a bolt-maintained byte slice.
		stmBytes = append(stmBytes, b.Bucket(taskMetaPath).Get(encodedID)...)

		if err := orgID.Decode(b.Bucket(orgByTaskID).Get(encodedID)); err != nil {
			return err
		}

		name = string(b.Bucket(nameByTaskID).Get(encodedID))
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	stm := backend.StoreTaskMeta{}
	if err := stm.Unmarshal(stmBytes); err != nil {
		return nil, nil, err
	}

	if stm.LatestCompleted < s.minLatestCompleted {
		stm.LatestCompleted = s.minLatestCompleted
		stm.AlignLatestCompleted()
	}

	return &backend.StoreTask{
		ID:     id,
		Org:    orgID,
		Name:   name,
		Script: script,
	}, &stm, nil
}

// DeleteTask deletes the task.
func (s *Store) DeleteTask(ctx context.Context, id platform.ID) (deleted bool, err error) {
	encodedID, err := id.Encode()
	if err != nil {
		return false, err
	}
	err = s.db.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		if check := b.Bucket(tasksPath).Get(encodedID); check == nil {
			return backend.ErrTaskNotFound
		}
		if err := b.Bucket(taskMetaPath).Delete(encodedID); err != nil {
			return err
		}
		if err := b.Bucket(tasksPath).Delete(encodedID); err != nil {
			return err
		}
		if err := b.Bucket(nameByTaskID).Delete(encodedID); err != nil {
			return err
		}

		org := b.Bucket(orgByTaskID).Get(encodedID)
		if len(org) > 0 {
			if err := b.Bucket(orgsPath).Bucket(org).Delete(encodedID); err != nil {
				return err
			}
		}
		return b.Bucket(orgByTaskID).Delete(encodedID)
	})
	if err != nil {
		if err == backend.ErrTaskNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) CreateNextRun(ctx context.Context, taskID platform.ID, now int64) (backend.RunCreation, error) {
	var rc backend.RunCreation

	encodedID, err := taskID.Encode()
	if err != nil {
		return rc, err
	}

	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		stmBytes := b.Bucket(taskMetaPath).Get(encodedID)
		if stmBytes == nil {
			return backend.ErrTaskNotFound
		}

		var stm backend.StoreTaskMeta
		err := stm.Unmarshal(stmBytes)
		if err != nil {
			return err
		}

		if stm.LatestCompleted < s.minLatestCompleted {
			stm.LatestCompleted = s.minLatestCompleted
			stm.AlignLatestCompleted()
		}

		rc, err = stm.CreateNextRun(now, func() (platform.ID, error) {
			return s.idGen.ID(), nil
		})
		if err != nil {
			return err
		}
		rc.Created.TaskID = taskID

		stmBytes, err = stm.Marshal()
		if err != nil {
			return err
		}
		return tx.Bucket(s.bucket).Bucket(taskMetaPath).Put(encodedID, stmBytes)
	}); err != nil {
		return backend.RunCreation{}, err
	}

	return rc, nil
}

// FinishRun removes runID from the list of running tasks and if its `now` is later then last completed update it.
func (s *Store) FinishRun(ctx context.Context, taskID, runID platform.ID) error {
	encodedID, err := taskID.Encode()
	if err != nil {
		return err
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		stmBytes := b.Bucket(taskMetaPath).Get(encodedID)
		var stm backend.StoreTaskMeta
		if err := stm.Unmarshal(stmBytes); err != nil {
			return err
		}
		if !stm.FinishRun(runID) {
			return ErrRunNotFound
		}

		stmBytes, err := stm.Marshal()
		if err != nil {
			return err
		}

		return tx.Bucket(s.bucket).Bucket(taskMetaPath).Put(encodedID, stmBytes)
	})
}

func (s *Store) ManuallyRunTimeRange(_ context.Context, taskID platform.ID, start, end, requestedAt int64) (*backend.StoreTaskMetaManualRun, error) {
	encodedID, err := taskID.Encode()
	if err != nil {
		return nil, err
	}
	var mRun *backend.StoreTaskMetaManualRun

	if err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		stmBytes := b.Bucket(taskMetaPath).Get(encodedID)
		var stm backend.StoreTaskMeta
		if err := stm.Unmarshal(stmBytes); err != nil {
			return err
		}
		makeID := func() (platform.ID, error) { return s.idGen.ID(), nil }
		if err := stm.ManuallyRunTimeRange(start, end, requestedAt, makeID); err != nil {
			return err
		}

		stmBytes, err := stm.Marshal()
		if err != nil {
			return err
		}
		mRun = stm.ManualRuns[len(stm.ManualRuns)-1]

		return tx.Bucket(s.bucket).Bucket(taskMetaPath).Put(encodedID, stmBytes)
	}); err != nil {
		return nil, err
	}
	return mRun, nil
}

// Close closes the store
func (s *Store) Close() error {
	return s.db.Close()
}

// DeleteOrg synchronously deletes an org and all their tasks from a bolt store.
func (s *Store) DeleteOrg(ctx context.Context, id platform.ID) error {
	orgID, err := id.Encode()
	if err != nil {
		return err
	}

	return s.db.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		ob := b.Bucket(orgsPath).Bucket(orgID)
		if ob == nil {
			return backend.ErrOrgNotFound
		}
		c := ob.Cursor()
		i := 0
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			i++
			// check for cancelation every 256 tasks deleted
			if i&0xFF == 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
			}
			if err := b.Bucket(tasksPath).Delete(k); err != nil {
				return err
			}
			if err := b.Bucket(taskMetaPath).Delete(k); err != nil {
				return err
			}
			if err := b.Bucket(orgByTaskID).Delete(k); err != nil {
				return err
			}
			if err := b.Bucket(nameByTaskID).Delete(k); err != nil {
				return err
			}
		}
		// check for cancelation one last time before we return
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return b.Bucket(orgsPath).DeleteBucket(orgID)
		}
	})
}
