package snap

import (
	"github.com/filecoin-project/lotus/curiosrc/ffi"
	"github.com/filecoin-project/lotus/curiosrc/seal"
	"github.com/filecoin-project/lotus/lib/harmony/harmonydb"
	"github.com/filecoin-project/lotus/lib/harmony/harmonytask"
	"github.com/filecoin-project/lotus/lib/harmony/resources"
)

type ProveTask struct {
	max int

	sc *ffi.SealCalls
	db *harmonydb.DB
}

func (p *ProveTask) Do(taskID harmonytask.TaskID, stillOwned func() bool) (done bool, err error) {
	//TODO implement me
	panic("implement me")
}

func (p *ProveTask) CanAccept(ids []harmonytask.TaskID, engine *harmonytask.TaskEngine) (*harmonytask.TaskID, error) {
	id := ids[0]
	return &id, nil
}

func (p *ProveTask) TypeDetails() harmonytask.TaskTypeDetails {
	gpu := 1.0
	if seal.IsDevnet {
		gpu = 0
	}
	return harmonytask.TaskTypeDetails{
		Max:  p.max,
		Name: "UpdateProve",
		Cost: resources.Resources{
			Cpu: 1,
			Gpu: gpu,
			Ram: 50 << 30, // todo correct value
		},
		MaxFailures: 3,
		IAmBored:    nil,
	}
}

func (p *ProveTask) Adder(taskFunc harmonytask.AddTaskFunc) {
	return
}

var _ harmonytask.TaskInterface = &ProveTask{}
