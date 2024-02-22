package lpseal

import (
	"bytes"
	"context"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-commp-utils/nonffi"
	"github.com/filecoin-project/go-commp-utils/zerocomm"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/crypto"

	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors/policy"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/lib/harmony/harmonydb"
	"github.com/filecoin-project/lotus/lib/harmony/harmonytask"
	"github.com/filecoin-project/lotus/lib/harmony/resources"
	"github.com/filecoin-project/lotus/provider/lpffi"
	"github.com/filecoin-project/lotus/storage/sealer/storiface"
)

var isDevnet = build.BlockDelaySecs < 30

type SDRAPI interface {
	ChainHead(context.Context) (*types.TipSet, error)
	StateGetRandomnessFromTickets(context.Context, crypto.DomainSeparationTag, abi.ChainEpoch, []byte, types.TipSetKey) (abi.Randomness, error)
}

type SDRTask struct {
	api SDRAPI
	db  *harmonydb.DB
	sp  *SealPoller

	sc *lpffi.SealCalls

	max int
}

func NewSDRTask(api SDRAPI, db *harmonydb.DB, sp *SealPoller, sc *lpffi.SealCalls, maxSDR int) *SDRTask {
	return &SDRTask{
		api: api,
		db:  db,
		sp:  sp,
		sc:  sc,
		max: maxSDR,
	}
}

type SDRTaskData struct {
	reservation *lpffi.StorageReservation
}

func (s *SDRTaskData) NotClaimed() error {
	if s.reservation != nil {
		s.reservation.Release()
		s.reservation = nil
	}

	return nil
}

func (s *SDRTask) Do(taskID harmonytask.TaskID, data harmonytask.AcceptData, stillOwned func() bool) (done bool, err error) {
	ctx := context.Background()

	acceptData, ok := data.(*SDRTaskData)
	if !ok {
		return false, xerrors.Errorf("invalid data type")
	}
	defer func() {
		if acceptData.reservation != nil {
			acceptData.reservation.Release()
			acceptData.reservation = nil
		}
	}()

	var sectorParamsArr []struct {
		SpID         int64                   `db:"sp_id"`
		SectorNumber int64                   `db:"sector_number"`
		RegSealProof abi.RegisteredSealProof `db:"reg_seal_proof"`
	}

	err = s.db.Select(ctx, &sectorParamsArr, `
		SELECT sp_id, sector_number, reg_seal_proof
		FROM sectors_sdr_pipeline
		WHERE task_id_sdr = $1`, taskID)
	if err != nil {
		return false, xerrors.Errorf("getting sector params: %w", err)
	}

	if len(sectorParamsArr) != 1 {
		return false, xerrors.Errorf("expected 1 sector params, got %d", len(sectorParamsArr))
	}
	sectorParams := sectorParamsArr[0]

	var pieces []struct {
		PieceIndex int64  `db:"piece_index"`
		PieceCID   string `db:"piece_cid"`
		PieceSize  int64  `db:"piece_size"`
	}

	err = s.db.Select(ctx, &pieces, `
		SELECT piece_index, piece_cid, piece_size
		FROM sectors_sdr_initial_pieces
		WHERE sp_id = $1 AND sector_number = $2 ORDER BY piece_index ASC`, sectorParams.SpID, sectorParams.SectorNumber)
	if err != nil {
		return false, xerrors.Errorf("getting pieces: %w", err)
	}

	ssize, err := sectorParams.RegSealProof.SectorSize()
	if err != nil {
		return false, xerrors.Errorf("getting sector size: %w", err)
	}

	var commd cid.Cid

	if len(pieces) > 0 {
		pieceInfos := make([]abi.PieceInfo, len(pieces))
		for i, p := range pieces {
			c, err := cid.Parse(p.PieceCID)
			if err != nil {
				return false, xerrors.Errorf("parsing piece cid: %w", err)
			}

			pieceInfos[i] = abi.PieceInfo{
				Size:     abi.PaddedPieceSize(p.PieceSize),
				PieceCID: c,
			}
		}

		commd, err = nonffi.GenerateUnsealedCID(sectorParams.RegSealProof, pieceInfos)
		if err != nil {
			return false, xerrors.Errorf("computing CommD: %w", err)
		}
	} else {
		commd = zerocomm.ZeroPieceCommitment(abi.PaddedPieceSize(ssize).Unpadded())
	}

	sref := storiface.SectorRef{
		ID: abi.SectorID{
			Miner:  abi.ActorID(sectorParams.SpID),
			Number: abi.SectorNumber(sectorParams.SectorNumber),
		},
		ProofType: sectorParams.RegSealProof,
	}

	// get ticket
	maddr, err := address.NewIDAddress(uint64(sectorParams.SpID))
	if err != nil {
		return false, xerrors.Errorf("getting miner address: %w", err)
	}

	// FAIL: api may be down
	// FAIL-RESP: rely on harmony retry
	ticket, ticketEpoch, err := s.getTicket(ctx, maddr)
	if err != nil {
		return false, xerrors.Errorf("getting ticket: %w", err)
	}

	// do the SDR!!

	// FAIL: storage may not have enough space
	// FAIL-RESP: rely on harmony retry

	// LATEFAIL: compute error in sdr
	// LATEFAIL-RESP: Check in Trees task should catch this; Will retry computing
	//                Trees; After one retry, it should return the sector to the
	// 			      SDR stage; max number of retries should be configurable

	err = s.sc.GenerateSDR(ctx, acceptData.reservation, sref, ticket, commd)
	if err != nil {
		return false, xerrors.Errorf("generating sdr: %w", err)
	}

	// store success!
	n, err := s.db.Exec(ctx, `UPDATE sectors_sdr_pipeline
		SET after_sdr = true, ticket_epoch = $3, ticket_value = $4
		WHERE sp_id = $1 AND sector_number = $2`,
		sectorParams.SpID, sectorParams.SectorNumber, ticketEpoch, []byte(ticket))
	if err != nil {
		return false, xerrors.Errorf("store sdr success: updating pipeline: %w", err)
	}
	if n != 1 {
		return false, xerrors.Errorf("store sdr success: updated %d rows", n)
	}

	return true, nil
}

func (s *SDRTask) getTicket(ctx context.Context, maddr address.Address) (abi.SealRandomness, abi.ChainEpoch, error) {
	ts, err := s.api.ChainHead(ctx)
	if err != nil {
		return nil, 0, xerrors.Errorf("getting chain head: %w", err)
	}

	ticketEpoch := ts.Height() - policy.SealRandomnessLookback
	buf := new(bytes.Buffer)
	if err := maddr.MarshalCBOR(buf); err != nil {
		return nil, 0, xerrors.Errorf("marshaling miner address: %w", err)
	}

	rand, err := s.api.StateGetRandomnessFromTickets(ctx, crypto.DomainSeparationTag_SealRandomness, ticketEpoch, buf.Bytes(), ts.Key())
	if err != nil {
		return nil, 0, xerrors.Errorf("getting randomness from tickets: %w", err)
	}

	return abi.SealRandomness(rand), ticketEpoch, nil
}

func (s *SDRTask) CanAccept(ids []harmonytask.TaskID, engine *harmonytask.TaskEngine) (*harmonytask.TaskID, harmonytask.AcceptData, error) {
	ctx := context.Background()

	for _, taskID := range ids {
		var sectorParamsArr []struct {
			SpID         int64                   `db:"sp_id"`
			SectorNumber int64                   `db:"sector_number"`
			RegSealProof abi.RegisteredSealProof `db:"reg_seal_proof"`
		}

		err := s.db.Select(ctx, &sectorParamsArr, `
		SELECT sp_id, sector_number, reg_seal_proof
		FROM sectors_sdr_pipeline
		WHERE task_id_sdr = $1`, taskID)
		if err != nil {
			log.Errorw("getting sector params", "error", err)
			continue
		}

		if len(sectorParamsArr) != 1 {
			log.Errorw("expected 1 sector params, got", "count", len(sectorParamsArr))
			continue
		}
		sectorParams := sectorParamsArr[0]

		sectorID := abi.SectorID{
			Miner:  abi.ActorID(sectorParams.SpID),
			Number: abi.SectorNumber(sectorParams.SectorNumber),
		}
		sectorRef := storiface.SectorRef{
			ID:        sectorID,
			ProofType: sectorParams.RegSealProof,
		}

		res, err := s.sc.ReserveSDRStorage(ctx, sectorRef)
		if err != nil {
			log.Errorw("reserving storage", "error", err)
			continue
		}

		return &taskID, &SDRTaskData{reservation: res}, nil
	}

	return nil, nil, nil
}

func (s *SDRTask) TypeDetails() harmonytask.TaskTypeDetails {
	res := harmonytask.TaskTypeDetails{
		Max:  s.max,
		Name: "SDR",
		Cost: resources.Resources{ // todo offset for prefetch?
			Cpu: 4, // todo multicore sdr
			Gpu: 0,
			Ram: 54 << 30,
		},
		MaxFailures: 2,
		Follows:     nil,
	}

	if isDevnet {
		res.Cost.Ram = 1 << 30
	}

	return res
}

func (s *SDRTask) Adder(taskFunc harmonytask.AddTaskFunc) {
	s.sp.pollers[pollerSDR].Set(taskFunc)
}

var _ harmonytask.TaskInterface = &SDRTask{}
