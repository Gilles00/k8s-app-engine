package etcd

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/Aptomi/aptomi/pkg/runtime"
	"github.com/Aptomi/aptomi/pkg/runtime/store"
	etcd "github.com/coreos/etcd/clientv3"
	etcdconc "github.com/coreos/etcd/clientv3/concurrency"
	"github.com/coreos/etcd/clientv3/namespace"
	"github.com/davecgh/go-spew/spew"
)

type etcdStore struct {
	client *etcd.Client
	types  *runtime.Types
	codec  store.Codec
}

func New(cfg Config, types *runtime.Types, codec store.Codec) (store.Interface, error) {
	if len(cfg.Endpoints) == 0 {
		cfg.Endpoints = []string{"localhost:2379"}
	}

	client, err := etcd.New(etcd.Config{
		Endpoints:            cfg.Endpoints,
		DialTimeout:          dialTimeout,
		DialKeepAliveTime:    keepaliveTime,
		DialKeepAliveTimeout: keepaliveTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("error while connecting to etcd: %s", err)
	}

	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	if cfg.Prefix != "" {
		cfg.Prefix = "/" + cfg.Prefix
		client.KV = namespace.NewKV(client.KV, cfg.Prefix)
		client.Lease = namespace.NewLease(client.Lease, cfg.Prefix)
		client.Watcher = namespace.NewWatcher(client.Watcher, cfg.Prefix)
	}

	// todo run compactor?

	return &etcdStore{
		client: client,
		types:  types,
		codec:  codec,
	}, nil
}

func (s *etcdStore) Close() error {
	return s.client.Close()
}

// todo need to rework keys to not include kind or to start with kind at least???

// Save saves Storable object with specified options into Etcd and updates indexes when appropriate.
// Workflow:
// 1. for non-versioned object key is always static, just put object into etcd and no indexes need to be updated (only
//    generation indexes currently exists)
// 2. for versioned object all manipulations are done inside a single transaction to guarantee atomic operations
//    (like index update, getting last existing generation or comparing with existing object), in addition to that
//    generation set for the object is always ignored if "forceGenOrReplace" option isn't used
// 3. if "replaceOrForceGen" option used, there should be non-zero generation set in the object, last generation will
//    not be checked in that case and old object will be removed from indexes, while new one will be added to them
// 4. default option is saving object with new generation if it differs from the last generation object (or first time
//    created), so, it'll only require adding object to indexes
func (s *etcdStore) Save(newStorable runtime.Storable, opts ...store.SaveOpt) error {
	if newStorable == nil {
		return fmt.Errorf("can't save nil")
	}

	saveOpts := store.NewSaveOpts(opts)
	info := s.types.Get(newStorable.GetKind())
	indexes := store.IndexesFor(info)
	key := "/" + runtime.KeyForStorable(newStorable)

	if !info.Versioned {
		data := s.marshal(newStorable)
		_, err := s.client.Put(context.TODO(), "/object"+key+"@"+runtime.LastOrEmptyGen.String(), string(data))
		return err
	}

	newObj := newStorable.(runtime.Versioned)
	// todo prefetch all needed keys for STM to maximize performance (in fact it'll get all data in one first request)
	// todo consider unmarshal to the info.New() to support gob w/o need to register types?
	_, err := etcdconc.NewSTM(s.client, func(stm etcdconc.STM) error {
		// need to remove this obj from indexes
		var prevObj runtime.Storable

		if saveOpts.IsReplaceOrForceGen() {
			newGen := newObj.GetGeneration()
			if newGen == runtime.LastOrEmptyGen {
				return fmt.Errorf("error while saving object %s with replaceOrForceGen option but with empty generation", key)
			}
			// need to check if there is an object already exists with gen from the object, if yes - remove it from indexes
			oldObjRaw := stm.Get("/object" + key + "@" + newGen.String())
			if oldObjRaw != "" {
				// todo avoid
				prevObj := info.New().(runtime.Storable)
				/*
					add field require not nil val for unmarshal field into codec
					if nil passed => create instance of desired object (w/o casting to storable) and pass to unmarshal
					if not nil => error if incorrect type
				*/
				s.unmarshal([]byte(oldObjRaw), prevObj)
			}

			// todo compare - if not changed - nothing to do
		} else {
			// need to get last gen using index, if exists - compare with, if different - increment revision and delete old from indexes
			lastGenRaw := stm.Get("/index/" + indexes.KeyForStorable(store.LastGenIndex, newStorable, s.codec))
			if lastGenRaw == "" {
				newObj.SetGeneration(runtime.FirstGen)
			} else {
				lastGen := s.unmarshalGen(lastGenRaw)
				oldObjRaw := stm.Get("/object" + key + "@" + lastGen.String())
				if oldObjRaw == "" {
					return fmt.Errorf("last gen index for %s seems to be corrupted: generation doesn't exist", key)
				}
				// todo avoid
				prevObj = info.New().(runtime.Storable)
				s.unmarshal([]byte(oldObjRaw), prevObj)
				if !reflect.DeepEqual(prevObj, newObj) {
					newObj.SetGeneration(lastGen.Next())
				} else {
					newObj.SetGeneration(lastGen)
					// nothing to do - object wasn't changed
					return nil
				}
			}
		}

		data := s.marshal(newObj)
		newGen := newObj.GetGeneration()
		stm.Put("/object"+key+"@"+newGen.String(), string(data))

		for _, index := range indexes.List {
			indexKey := "/index/" + index.KeyForStorable(newStorable, s.codec)
			if index.Type == store.IndexTypeLastGen {
				stm.Put(indexKey, s.marshalGen(newGen))
			} else if index.Type == store.IndexTypeListGen {
				if prevObj != nil {
					// todo delete old obj from indexes
				}

				valueList := &store.IndexValueList{}
				valueListRaw := stm.Get(indexKey)
				if valueListRaw != "" {
					s.unmarshal([]byte(valueListRaw), valueList)
				}
				// todo avoid marshaling gens for indexes by using special index value list type for gens
				valueList.Add([]byte(s.marshalGen(newGen)))

				data := s.marshal(valueList)

				stm.Put(indexKey, string(data))
			} else {
				panic("only indexes with types store.IndexTypeLastGen and store.IndexTypeListGen are currently supported by Etcd store")
			}
		}

		return nil
	})

	return err
}

/*
Current Find use cases:

* Find(kind, keyPrefix)
* Find(kind, key, gen)  (gen=0 for non-versioned)
* Find(kind, key, WithWhereEq)
* Find(kind, key, WithWhereEq, WithGetFirst)
* Find(kind, key, WithWhereEq, WithGetLast)

\\ summary: keyPrefix OR key+gen OR key + whereEq+list/first/last

Workflow:
* validate parameters and result
* identify requested list or one(first or last)
* build list of keys that are result (could be just build key from parameters or use index)
* based on requested list/first/last get corresponding element from the key list and query value for it

*/
func (s *etcdStore) Find(kind runtime.Kind, result interface{}, opts ...store.FindOpt) error {
	findOpts := store.NewFindOpts(opts)
	info := s.types.Get(kind)

	resultTypeSingle := reflect.TypeOf(info.New())
	resultTypeList := reflect.PtrTo(reflect.SliceOf(resultTypeSingle))

	resultList := false

	resultType := reflect.TypeOf(result)
	if resultType == resultTypeSingle {
		// ok!
	} else if resultType == resultTypeList {
		// ok!
		resultList = true
	} else {
		return fmt.Errorf("result should be %s or %s, but found: %s", resultTypeSingle, resultTypeList, resultType)
	}

	fmt.Println("findOpts: ", spew.Sdump(findOpts))
	fmt.Println("resultList: ", resultList)

	v := reflect.ValueOf(result).Elem()
	if findOpts.GetKeyPrefix() != "" {
		return s.findByKeyPrefix(findOpts, info, func(elem interface{}) {
			// todo validate type of the elem
			v.Set(reflect.Append(v, reflect.ValueOf(elem)))
		})
	} else if findOpts.GetKey() != "" && findOpts.GetFieldEqName() == "" {
		return s.findByKey(findOpts, info, func(elem interface{}) {
			// todo validate type of the elem
			v.Set(reflect.ValueOf(elem))
		})
	} else {
		return s.findByPredicate(findOpts, info, func(elem interface{}) {
			// todo validate type of the elem
			v.Set(reflect.Append(v, reflect.ValueOf(elem)))
		})
	}
}

func (s *etcdStore) findByKeyPrefix(opts *store.FindOpts, info *runtime.TypeInfo, addToResult func(interface{})) error {
	return nil
}

func (s *etcdStore) findByKey(opts *store.FindOpts, info *runtime.TypeInfo, addToResult func(interface{})) error {
	return nil
}

func (s *etcdStore) findByPredicate(opts *store.FindOpts, info *runtime.TypeInfo, addToResult func(interface{})) error {
	addToResult(info.New())
	addToResult(info.New())
	return nil
}

func (s *etcdStore) Delete(kind runtime.Kind, key runtime.Key) error {
	panic("implement me")
}
