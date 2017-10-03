package lang

import (
	"fmt"
	"github.com/Aptomi/aptomi/pkg/slinga/object"
	"github.com/Aptomi/aptomi/pkg/slinga/object/codec"
	"github.com/Aptomi/aptomi/pkg/slinga/object/codec/yaml"
	"github.com/mattn/go-zglob"
	"io/ioutil"
	"path/filepath"
	"sort"
	"strings"
)

func LoadUnitTestsPolicy(storeDir string) *Policy {
	loader := NewFileLoader(storeDir)

	policy := NewPolicy()
	objects, err := loader.LoadObjects()
	if err != nil {
		panic(fmt.Sprintf("Error while loading test policy: %s", err))
	}

	for _, obj := range objects {
		policy.AddObject(obj)
	}

	return policy
}

func NewFileLoader(path string) *FileLoader {
	catalog := object.NewObjectCatalog(ServiceObject, ContractObject, ClusterObject, RuleObject, DependencyObject)

	return &FileLoader{yaml.NewCodec(catalog), path}
}

type FileLoader struct {
	codec codec.MarshalUnmarshaler
	path  string
}

func (store *FileLoader) LoadObjects() ([]object.Base, error) {
	files, _ := zglob.Glob(filepath.Join(store.path, "**", "*.yaml"))
	sort.Strings(files)

	result := make([]object.Base, 0)
	for _, f := range files {
		if !strings.Contains(f, "external") {
			objects, err := store.loadObjectsFromFile(f)
			if err != nil {
				return nil, fmt.Errorf("Error while loading objects from file %s: %s", f, err)
			}

			result = append(result, objects...)
		}
	}

	// This hack is needed to make sure that we'll get test data in the same way like after marshaling objects
	// and storing them in DB. Example: empty fields will be stored anyway, while we omitting them in test data.
	data, err := store.codec.MarshalMany(result)
	if err != nil {
		return nil, fmt.Errorf("Error marshaling loaded objects: %s", err)
	}
	result, err = store.codec.UnmarshalOneOrMany(data)
	if err != nil {
		return nil, fmt.Errorf("Error unmarshaling loaded objects: %s", err)
	}

	return result, nil
}

func (store *FileLoader) loadObjectsFromFile(path string) ([]object.Base, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("Error while reading file %s: %s", path, err)
	}
	objects, err := store.codec.UnmarshalOneOrMany(data)
	if err != nil {
		return nil, fmt.Errorf("Error while unmarshaling file %s: %s", path, err)
	}

	return objects, nil
}