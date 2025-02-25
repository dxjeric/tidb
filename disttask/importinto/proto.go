// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package importinto

import (
	"fmt"
	"sync"

	"github.com/pingcap/tidb/br/pkg/lightning/backend"
	"github.com/pingcap/tidb/br/pkg/lightning/backend/external"
	"github.com/pingcap/tidb/br/pkg/lightning/mydump"
	"github.com/pingcap/tidb/br/pkg/lightning/verification"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/executor/asyncloaddata"
	"github.com/pingcap/tidb/executor/importer"
	"github.com/pingcap/tidb/meta/autoid"
)

// Steps of IMPORT INTO, each step is represented by one or multiple subtasks.
// the initial step is StepInit(-1)
// steps are processed in the following order:
// - local sort: StepInit -> StepImport -> StepPostProcess -> StepDone
// - global sort: StepInit -> StepEncodeAndSort -> StepWriteAndIngest -> StepPostProcess -> StepDone
const (
	// StepImport we sort source data and ingest it into TiKV in this step.
	StepImport int64 = 1
	// StepPostProcess we verify checksum and add index in this step.
	StepPostProcess int64 = 2
	// StepEncodeAndSort encode source data and write sorted kv into global storage.
	StepEncodeAndSort int64 = 3
	// StepWriteAndIngest write sorted kv into TiKV and ingest it.
	StepWriteAndIngest int64 = 4
)

// TaskMeta is the task of IMPORT INTO.
// All the field should be serializable.
type TaskMeta struct {
	// IMPORT INTO job id.
	JobID  int64
	Plan   importer.Plan
	Stmt   string
	Result Result
	// eligible instances to run this task, we run on all instances if it's empty.
	// we only need this when run IMPORT INTO without distributed option now, i.e.
	// running on the instance that initiate the IMPORT INTO.
	EligibleInstances []*infosync.ServerInfo
	// the file chunks to import, when import from server file, we need to pass those
	// files to the framework dispatcher which might run on another instance.
	// we use a map from engine ID to chunks since we need support split_file for CSV,
	// so need to split them into engines before passing to dispatcher.
	ChunkMap map[int32][]Chunk
}

// ImportStepMeta is the meta of import step.
// Dispatcher will split the task into subtasks(FileInfos -> Chunks)
// All the field should be serializable.
type ImportStepMeta struct {
	// this is the engine ID, not the id in tidb_background_subtask table.
	ID       int32
	Chunks   []Chunk
	Checksum Checksum
	Result   Result
	// MaxIDs stores the max id that have been used during encoding for each allocator type.
	// the max id is same among all allocator types for now, since we're using same base, see
	// NewPanickingAllocators for more info.
	MaxIDs map[autoid.AllocatorType]int64

	SortedDataMeta *external.SortedKVMeta
	// SortedIndexMetas is a map from index id to its sorted kv meta.
	SortedIndexMetas map[int64]*external.SortedKVMeta
}

const (
	dataKVGroup = "data"
)

// WriteIngestStepMeta is the meta of write and ingest step.
// only used when global sort is enabled.
type WriteIngestStepMeta struct {
	KVGroup               string `json:"kv_group"`
	external.SortedKVMeta `json:",inline"`
	RangeSplitKeys        [][]byte `json:"range_split_keys"`

	Result Result
}

// PostProcessStepMeta is the meta of post process step.
type PostProcessStepMeta struct {
	// accumulated checksum of all subtasks in import step.
	Checksum Checksum
	// MaxIDs of max all max-ids of subtasks in import step.
	MaxIDs map[autoid.AllocatorType]int64
}

// SharedVars is the shared variables of all minimal tasks in a subtask.
// This is because subtasks cannot directly obtain the results of the minimal subtask.
// All the fields should be concurrent safe.
type SharedVars struct {
	TableImporter *importer.TableImporter
	DataEngine    *backend.OpenedEngine
	IndexEngine   *backend.OpenedEngine
	Progress      *asyncloaddata.Progress

	mu       sync.Mutex
	Checksum *verification.KVChecksum

	SortedDataMeta *external.SortedKVMeta
	// SortedIndexMetas is a map from index id to its sorted kv meta.
	SortedIndexMetas map[int64]*external.SortedKVMeta
}

func (sv *SharedVars) mergeDataSummary(summary *external.WriterSummary) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.SortedDataMeta.MergeSummary(summary)
}

func (sv *SharedVars) mergeIndexSummary(indexID int64, summary *external.WriterSummary) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	meta, ok := sv.SortedIndexMetas[indexID]
	if !ok {
		meta = external.NewSortedKVMeta(summary)
		sv.SortedIndexMetas[indexID] = meta
		return
	}
	meta.MergeSummary(summary)
}

// importStepMinimalTask is the minimal task of IMPORT INTO.
// Scheduler will split the subtask into minimal tasks(Chunks -> Chunk)
type importStepMinimalTask struct {
	Plan       importer.Plan
	Chunk      Chunk
	SharedVars *SharedVars
}

// IsMinimalTask implements the MinimalTask interface.
func (*importStepMinimalTask) IsMinimalTask() {}

func (t *importStepMinimalTask) String() string {
	return fmt.Sprintf("chunk:%s:%d", t.Chunk.Path, t.Chunk.Offset)
}

// Chunk records the chunk information.
type Chunk struct {
	Path         string
	FileSize     int64
	Offset       int64
	EndOffset    int64
	PrevRowIDMax int64
	RowIDMax     int64
	Type         mydump.SourceType
	Compression  mydump.Compression
	Timestamp    int64
}

// Checksum records the checksum information.
type Checksum struct {
	Sum  uint64
	KVs  uint64
	Size uint64
}

// Result records the metrics information.
// This portion of the code may be implemented uniformly in the framework in the future.
type Result struct {
	ReadRowCnt   uint64
	LoadedRowCnt uint64
	ColSizeMap   map[int64]int64
}
