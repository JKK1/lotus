package versions

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"

	badger "github.com/dgraph-io/badger/v4"
	badgerV4 "github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/ristretto"
	"github.com/dgraph-io/ristretto/z"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"
)

// BadgerV4 wraps the Badger v4 database to implement the BadgerDB interface.
type BadgerV4 struct {
	*badger.DB
}

func (b *BadgerV4) Close() error {
	return b.DB.Close()
}

func (b *BadgerV4) IsClosed() bool {
	return b.DB.IsClosed()
}

func (b *BadgerV4) NewStream() BadgerStream {
	return &BadgerV4Stream{b.DB.NewStream()}
}

func (b *BadgerV4) Update(fn func(txn Txn) error) error {
	return b.DB.Update(func(txn *badger.Txn) error {
		return fn(&BadgerV4Txn{txn})
	})
}

func (b *BadgerV4) View(fn func(txn Txn) error) error {
	return b.DB.View(func(txn *badger.Txn) error {
		return fn(&BadgerV4Txn{txn})
	})
}

func (b *BadgerV4) NewTransaction(update bool) Txn {
	return &BadgerV4Txn{b.DB.NewTransaction(update)}
}

func (b *BadgerV4) RunValueLogGC(discardRatio float64) error {
	return b.DB.RunValueLogGC(discardRatio)
}

func (b *BadgerV4) Sync() error {
	return b.DB.Sync()
}

func (b *BadgerV4) MaxBatchCount() int64 {
	return b.DB.MaxBatchCount()
}

func (b *BadgerV4) MaxBatchSize() int64 {
	return b.DB.MaxBatchSize()
}

func (b *BadgerV4) IndexCacheMetrics() *ristretto.Metrics {
	return b.DB.IndexCacheMetrics()
}

func (b *BadgerV4) GetErrKeyNotFound() error {
	return badger.ErrKeyNotFound
}

func (b *BadgerV4) GetErrNoRewrite() error {
	return badger.ErrNoRewrite
}

func (b *BadgerV4) NewWriteBatch() WriteBatch {
	return &BadgerV4WriteBatch{b.DB.NewWriteBatch()}
}

func (b *BadgerV4) Flatten(workers int) error {
	return b.DB.Flatten(workers)
}

func (b *BadgerV4) Size() (lsm int64, vlog int64) {
	return b.DB.Size()
}

func (b *BadgerV4) Copy(to BadgerDB) error {
	stream := b.DB.NewStream()
	stream.LogPrefix = "doCopy"
	stream.NumGo = clamp(runtime.NumCPU()/2, 2, 8)
	stream.Send = func(buf *z.Buffer) error {
		list, err := badger.BufferToKVList(buf)
		if err != nil {
			return fmt.Errorf("buffer to KV list conversion: %w", err)
		}

		batch := to.NewWriteBatch()
		defer batch.Cancel()

		for _, kv := range list.Kv {
			if kv.Key == nil || kv.Value == nil {
				continue
			}
			if err := batch.Set(kv.Key, kv.Value); err != nil {
				return err
			}
		}

		return batch.Flush()
	}

	return stream.Orchestrate(context.Background())
}

func (b *BadgerV4) DefaultOptions(path string, readonly bool) Options {
	var opts Options
	bopts := badgerV4.DefaultOptions(path)
	bopts.ReadOnly = readonly

	// Envvar LOTUS_CHAIN_BADGERSTORE_COMPACTIONWORKERNUM
	if badgerNumCompactors, badgerNumCompactorsSet := os.LookupEnv("LOTUS_CHAIN_BADGERSTORE_COMPACTIONWORKERNUM"); badgerNumCompactorsSet {
		if numWorkers, err := strconv.Atoi(badgerNumCompactors); err == nil && numWorkers >= 0 {
			bopts.NumCompactors = numWorkers
		}
	}
	opts.V4Options = bopts
	return opts

}

func (b *BadgerV4) Load(r io.Reader, maxPendingWrites int) error {
	return b.DB.Load(r, maxPendingWrites)
}

func (b *BadgerV4) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	return nil, fmt.Errorf("AllKeysChan is not implemented")
}

func (b *BadgerV4) DeleteBlock(context.Context, cid.Cid) error {
	return fmt.Errorf("DeleteBlock is not implemented")
}

type BadgerV4WriteBatch struct {
	*badger.WriteBatch
}

func (wb *BadgerV4WriteBatch) Set(key, val []byte) error {
	return wb.WriteBatch.Set(key, val)
}

func (wb *BadgerV4WriteBatch) Delete(key []byte) error {
	return wb.WriteBatch.Delete(key)
}

func (wb *BadgerV4WriteBatch) Flush() error {
	return wb.WriteBatch.Flush()
}

func (wb *BadgerV4WriteBatch) Cancel() {
	wb.WriteBatch.Cancel()
}

type BadgerV4Stream struct {
	*badger.Stream
}

func (s *BadgerV4Stream) SetNumGo(numGo int) {
	s.Stream.NumGo = numGo
}

func (s *BadgerV4Stream) SetLogPrefix(prefix string) {
	s.Stream.LogPrefix = prefix
}
func (s *BadgerV4Stream) ForEach(ctx context.Context, fn func(key string, value string) error) error {
	s.Stream.Send = func(buf *z.Buffer) error {
		list, err := badger.BufferToKVList(buf)
		if err != nil {
			return fmt.Errorf("buffer to KV list conversion: %w", err)
		}
		for _, kv := range list.Kv {
			if kv.Key == nil || kv.Value == nil {
				continue
			}
			err := fn(string(kv.Key), string(kv.Value))
			if err != nil {
				return xerrors.Errorf("foreach function: %w", err)
			}

		}
		return nil
	}
	if err := s.Orchestrate(ctx); err != nil {
		return xerrors.Errorf("orchestrate stream: %w", err)
	}
	return nil
}

func (s *BadgerV4Stream) Orchestrate(ctx context.Context) error {
	return s.Stream.Orchestrate(ctx)
}

type BadgerV4Txn struct {
	*badger.Txn
}

func (txn *BadgerV4Txn) Get(key []byte) (Item, error) {
	item, err := txn.Txn.Get(key)
	return &BadgerV4Item{item}, err
}

func (txn *BadgerV4Txn) Set(key, val []byte) error {
	return txn.Txn.Set(key, val)
}

func (txn *BadgerV4Txn) Delete(key []byte) error {
	return txn.Txn.Delete(key)
}

func (txn *BadgerV4Txn) Commit() error {
	return txn.Txn.Commit()
}

func (txn *BadgerV4Txn) Discard() {
	txn.Txn.Discard()
}

func (txn *BadgerV4Txn) NewIterator(opts IteratorOptions) Iterator {
	badgerOpts := badger.DefaultIteratorOptions
	badgerOpts.PrefetchSize = opts.PrefetchSize
	badgerOpts.Prefix = opts.Prefix
	return &BadgerV4Iterator{txn.Txn.NewIterator(badgerOpts)}
}

type BadgerV4Iterator struct {
	*badger.Iterator
}

func (it *BadgerV4Iterator) Next()           { it.Iterator.Next() }
func (it *BadgerV4Iterator) Rewind()         { it.Iterator.Rewind() }
func (it *BadgerV4Iterator) Seek(key []byte) { it.Iterator.Seek(key) }
func (it *BadgerV4Iterator) Close()          { it.Iterator.Close() }
func (it *BadgerV4Iterator) Item() Item      { return &BadgerV4Item{it.Iterator.Item()} }
func (it *BadgerV4Iterator) Valid() bool     { return it.Iterator.Valid() }

type BadgerV4Item struct {
	*badger.Item
}

func (item *BadgerV4Item) Value(fn func([]byte) error) error {
	return item.Item.Value(fn)
}

func (item *BadgerV4Item) Key() []byte {
	return item.Item.Key()
}

func (item *BadgerV4Item) Version() uint64 {
	return item.Item.Version()
}

func (item *BadgerV4Item) ValueCopy(dst []byte) ([]byte, error) {
	return item.Item.ValueCopy(dst)
}

func (item *BadgerV4Item) ValueSize() int64 {
	return item.Item.ValueSize()
}