package datastore

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/libnetwork/discoverapi"
	store "github.com/docker/docker/libnetwork/internal/kvstore"
	"github.com/docker/docker/libnetwork/internal/kvstore/boltdb"
	"github.com/docker/docker/libnetwork/scope"
	"github.com/docker/docker/libnetwork/types"
)

// ErrKeyModified is raised for an atomic update when the update is working on a stale state
var (
	ErrKeyModified = store.ErrKeyModified
	ErrKeyNotFound = store.ErrKeyNotFound
)

type Store struct {
	mu    sync.Mutex
	scope string
	store store.Store
	cache *cache
}

// KVObject is Key/Value interface used by objects to be part of the Store.
type KVObject interface {
	// Key method lets an object provide the Key to be used in KV Store
	Key() []string
	// KeyPrefix method lets an object return immediate parent key that can be used for tree walk
	KeyPrefix() []string
	// Value method lets an object marshal its content to be stored in the KV store
	Value() []byte
	// SetValue is used by the datastore to set the object's value when loaded from the data store.
	SetValue([]byte) error
	// Index method returns the latest DB Index as seen by the object
	Index() uint64
	// SetIndex method allows the datastore to store the latest DB Index into the object
	SetIndex(uint64)
	// Exists returns true if the object exists in the datastore, false if it hasn't been stored yet.
	// When SetIndex() is called, the object has been stored.
	Exists() bool
	// DataScope indicates the storage scope of the KV object
	DataScope() string
	// Skip provides a way for a KV Object to avoid persisting it in the KV Store
	Skip() bool
}

// KVConstructor interface defines methods which can construct a KVObject from another.
type KVConstructor interface {
	// New returns a new object which is created based on the
	// source object
	New() KVObject
	// CopyTo deep copies the contents of the implementing object
	// to the passed destination object
	CopyTo(KVObject) error
}

// ScopeCfg represents Datastore configuration.
type ScopeCfg struct {
	Client ScopeClientCfg
}

// ScopeClientCfg represents Datastore Client-only mode configuration
type ScopeClientCfg struct {
	Provider string
	Address  string
	Config   *store.Config
}

const (
	// LocalScope indicates to store the KV object in local datastore such as boltdb
	//
	// Deprecated: use [scope.Local].
	LocalScope = scope.Local
	// GlobalScope indicates to store the KV object in global datastore
	//
	// Deprecated: use [scope.Global].
	GlobalScope = scope.Global
	// SwarmScope is not indicating a datastore location. It is defined here
	// along with the other two scopes just for consistency.
	//
	// Deprecated: use [scope.Swarm].
	SwarmScope = scope.Swarm
)

const (
	// NetworkKeyPrefix is the prefix for network key in the kv store
	NetworkKeyPrefix = "network"
	// EndpointKeyPrefix is the prefix for endpoint key in the kv store
	EndpointKeyPrefix = "endpoint"
)

var (
	defaultRootChain = []string{"docker", "network", "v1.0"}
	rootChain        = defaultRootChain
)

const defaultPrefix = "/var/lib/docker/network/files"

// DefaultScope returns a default scope config for clients to use.
func DefaultScope(dataDir string) ScopeCfg {
	var dbpath string
	if dataDir == "" {
		dbpath = defaultPrefix + "/local-kv.db"
	} else {
		dbpath = dataDir + "/network/files/local-kv.db"
	}

	return ScopeCfg{
		Client: ScopeClientCfg{
			Provider: string(store.BOLTDB),
			Address:  dbpath,
			Config: &store.Config{
				Bucket:            "libnetwork",
				ConnectionTimeout: time.Minute,
			},
		},
	}
}

// IsValid checks if the scope config has valid configuration.
func (cfg *ScopeCfg) IsValid() bool {
	if cfg == nil || strings.TrimSpace(cfg.Client.Provider) == "" || strings.TrimSpace(cfg.Client.Address) == "" {
		return false
	}

	return true
}

// Key provides convenient method to create a Key
func Key(key ...string) string {
	var b strings.Builder
	for _, parts := range [][]string{rootChain, key} {
		for _, part := range parts {
			b.WriteString(part)
			b.WriteString("/")
		}
	}
	return b.String()
}

// newClient used to connect to KV Store
func newClient(kv string, addr string, config *store.Config) (*Store, error) {
	if kv != string(store.BOLTDB) {
		return nil, fmt.Errorf("unsupported KV store")
	}

	if config == nil {
		config = &store.Config{}
	}

	// Parse file path
	s, err := boltdb.New(strings.Split(addr, ","), config)
	if err != nil {
		return nil, err
	}

	ds := &Store{scope: scope.Local, store: s}
	ds.cache = newCache(ds)

	return ds, nil
}

// New creates a new Store instance.
func New(cfg ScopeCfg) (*Store, error) {
	if cfg.Client.Provider == "" || cfg.Client.Address == "" {
		cfg = DefaultScope("")
	}

	return newClient(cfg.Client.Provider, cfg.Client.Address, cfg.Client.Config)
}

// FromConfig creates a new instance of LibKV data store starting from the datastore config data.
func FromConfig(dsc discoverapi.DatastoreConfigData) (*Store, error) {
	var (
		ok    bool
		sCfgP *store.Config
	)

	sCfgP, ok = dsc.Config.(*store.Config)
	if !ok && dsc.Config != nil {
		return nil, fmt.Errorf("cannot parse store configuration: %v", dsc.Config)
	}

	ds, err := New(ScopeCfg{
		Client: ScopeClientCfg{
			Address:  dsc.Address,
			Provider: dsc.Provider,
			Config:   sCfgP,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to construct datastore client from datastore configuration %v: %v", dsc, err)
	}

	return ds, err
}

// Close closes the data store.
func (ds *Store) Close() {
	ds.store.Close()
}

// Scope returns the scope of the store.
func (ds *Store) Scope() string {
	return ds.scope
}

// PutObjectAtomic provides an atomic add and update operation for a Record.
func (ds *Store) PutObjectAtomic(kvObject KVObject) error {
	var (
		previous *store.KVPair
		pair     *store.KVPair
		err      error
	)
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if kvObject == nil {
		return types.BadRequestErrorf("invalid KV Object : nil")
	}

	kvObjValue := kvObject.Value()

	if kvObjValue == nil {
		return types.BadRequestErrorf("invalid KV Object with a nil Value for key %s", Key(kvObject.Key()...))
	}

	if kvObject.Skip() {
		goto add_cache
	}

	if kvObject.Exists() {
		previous = &store.KVPair{Key: Key(kvObject.Key()...), LastIndex: kvObject.Index()}
	} else {
		previous = nil
	}

	pair, err = ds.store.AtomicPut(Key(kvObject.Key()...), kvObjValue, previous)
	if err != nil {
		if err == store.ErrKeyExists {
			return ErrKeyModified
		}
		return err
	}

	kvObject.SetIndex(pair.LastIndex)

add_cache:
	if ds.cache != nil {
		// If persistent store is skipped, sequencing needs to
		// happen in cache.
		return ds.cache.add(kvObject, kvObject.Skip())
	}

	return nil
}

// GetObject gets data from the store and unmarshals to the specified object.
func (ds *Store) GetObject(key string, o KVObject) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if ds.cache != nil {
		return ds.cache.get(o)
	}

	kvPair, err := ds.store.Get(key)
	if err != nil {
		return err
	}

	if err := o.SetValue(kvPair.Value); err != nil {
		return err
	}

	// Make sure the object has a correct view of the DB index in
	// case we need to modify it and update the DB.
	o.SetIndex(kvPair.LastIndex)
	return nil
}

func (ds *Store) ensureParent(parent string) error {
	exists, err := ds.store.Exists(parent)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return ds.store.Put(parent, []byte{})
}

// List returns of a list of KVObjects belonging to the parent key. The caller
// must pass a KVObject of the same type as the objects that need to be listed.
func (ds *Store) List(key string, kvObject KVObject) ([]KVObject, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if ds.cache != nil {
		return ds.cache.list(kvObject)
	}

	var kvol []KVObject
	cb := func(key string, val KVObject) {
		kvol = append(kvol, val)
	}
	err := ds.iterateKVPairsFromStore(key, kvObject, cb)
	if err != nil {
		return nil, err
	}
	return kvol, nil
}

func (ds *Store) iterateKVPairsFromStore(key string, kvObject KVObject, callback func(string, KVObject)) error {
	// Bail out right away if the kvObject does not implement KVConstructor
	ctor, ok := kvObject.(KVConstructor)
	if !ok {
		return fmt.Errorf("error listing objects, object does not implement KVConstructor interface")
	}

	// Make sure the parent key exists
	if err := ds.ensureParent(key); err != nil {
		return err
	}

	kvList, err := ds.store.List(key)
	if err != nil {
		return err
	}

	for _, kvPair := range kvList {
		if len(kvPair.Value) == 0 {
			continue
		}

		dstO := ctor.New()
		if err := dstO.SetValue(kvPair.Value); err != nil {
			return err
		}

		// Make sure the object has a correct view of the DB index in
		// case we need to modify it and update the DB.
		dstO.SetIndex(kvPair.LastIndex)
		callback(kvPair.Key, dstO)
	}

	return nil
}

// Map returns a Map of KVObjects.
func (ds *Store) Map(key string, kvObject KVObject) (map[string]KVObject, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	kvol := make(map[string]KVObject)
	cb := func(key string, val KVObject) {
		// Trim the leading & trailing "/" to make it consistent across all stores
		kvol[strings.Trim(key, "/")] = val
	}
	err := ds.iterateKVPairsFromStore(key, kvObject, cb)
	if err != nil {
		return nil, err
	}
	return kvol, nil
}

// DeleteObjectAtomic performs atomic delete on a record.
func (ds *Store) DeleteObjectAtomic(kvObject KVObject) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if kvObject == nil {
		return types.BadRequestErrorf("invalid KV Object : nil")
	}

	previous := &store.KVPair{Key: Key(kvObject.Key()...), LastIndex: kvObject.Index()}

	if kvObject.Skip() {
		goto deleteCache
	}

	if err := ds.store.AtomicDelete(Key(kvObject.Key()...), previous); err != nil {
		if err == store.ErrKeyExists {
			return ErrKeyModified
		}
		return err
	}

deleteCache:
	// cleanup the cache only if AtomicDelete went through successfully
	if ds.cache != nil {
		// If persistent store is skipped, sequencing needs to
		// happen in cache.
		return ds.cache.del(kvObject, kvObject.Skip())
	}

	return nil
}
