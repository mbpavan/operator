/*
Copyright 2021 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"encoding/json"
	"fmt"

	mf "github.com/manifestival/manifestival"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type VersionError error

var (
	errConfigMap VersionError = fmt.Errorf("version information could not be determined from ConfigMap")
)

func IsFetchVersionError(err error) bool {
	return err == errConfigMap
}

// FetchVersionFromConfigMap finds the component version from the ConfigMap data field. It looks
// for the version key in the ConfigMap and if the ConfigMap or version key is not found
// then return the error.
func FetchVersionFromConfigMap(manifest mf.Manifest, configMapName string) (string, error) {
	configMaps := manifest.Filter(mf.ByKind("ConfigMap"), mf.ByName(configMapName))

	if len(configMaps.Resources()) == 0 {
		return "", errConfigMap
	}

	versionConfigMap := configMaps.Resources()[0]
	dataObj, _, _ := unstructured.NestedStringMap(versionConfigMap.Object, "data")
	version := dataObj["version"]

	if version != "" {
		return version, nil
	}

	return "", errConfigMap
}

// converts struct to map with json encoding
func StructToMap(in, out interface{}) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// Helper function to serialize labels map to JSON string
func SerializeLabelsToJSON(labels map[string]string) (string, error) {
	bytes, err := json.Marshal(labels)
	if err != nil {
		return "", fmt.Errorf("failed to serialize labels to JSON: %v", err)
	}
	return string(bytes), nil
}
