// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package config

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/text/transform"
	"gopkg.in/yaml.v2"
)

type yamlConfigProvider struct {
	root yamlNode
}

var (
	_envSeparator = ":"
	_emptyDefault = `""`
)

func newYAMLProviderCore(files ...io.Reader) (*yamlConfigProvider, error) {
	var root interface{}
	for _, v := range files {
		var curr interface{}
		if err := unmarshalYAMLValue(v, &curr); err != nil {
			if file, ok := v.(*os.File); ok {
				return nil, errors.Wrapf(err, "in file: %q", file.Name())
			}

			return nil, err
		}

		tmp, err := mergeMaps(root, curr)
		if err != nil {
			return nil, err
		}

		root = tmp
	}

	return &yamlConfigProvider{
		root: yamlNode{
			nodeType: getNodeType(root),
			key:      Root,
			value:    root,
		},
	}, nil
}

// We need to have a custom merge map because yamlV2 doesn't unmarshal
// `map[interface{}]map[interface{}]interface{}` as we expect: it will
// replace second level maps with new maps on each unmarshal call,
// instead of merging them.
//
// The merge strategy for two objects A and B is following:
// If A and B are maps, A and B will form a new map with keys from A and B and
// values from B will overwrite values of A. e.g.:
//   A:                B:                 merge(A, B):
//     keep:A            new:B              keep:A
//     update:fromA      update:fromB       update:fromB
//                                          new:B
//
// If A is a map and B is not, this function will return an error,
// e.g. key:value and -slice.
//
// In all the remaining cases B will overwrite A.
func mergeMaps(dst interface{}, src interface{}) (interface{}, error) {
	if dst == nil {
		return src, nil
	}

	if src == nil {
		return dst, nil
	}

	switch s := src.(type) {
	case map[interface{}]interface{}:
		dstMap, ok := dst.(map[interface{}]interface{})
		if !ok {
			return nil, fmt.Errorf(
				"can't merge map[interface{}]interface{} and %T. Source: %q. Destination: %q",
				dst,
				src,
				dst)
		}

		for k, v := range s {
			oldVal := dstMap[k]
			if oldVal == nil {
				dstMap[k] = v
			} else {
				tmp, err := mergeMaps(oldVal, v)
				if err != nil {
					return nil, err
				}

				dstMap[k] = tmp
			}
		}
	default:
		dst = src
	}

	return dst, nil
}

// NewYAMLProviderFromFiles creates a configuration provider from a set of YAML
// file names. All the objects are going to be merged and arrays/values
// overridden in the order of the files.
func NewYAMLProviderFromFiles(files ...string) (Provider, error) {
	readClosers, err := filesToReaders(files...)
	if err != nil {
		return nil, err
	}

	readers := make([]io.Reader, len(readClosers))
	for i, r := range readClosers {
		readers[i] = r
	}

	provider, err := NewYAMLProviderFromReader(readers...)

	for _, r := range readClosers {
		nerr := r.Close()
		if err == nil {
			err = nerr
		}
	}

	return provider, err
}

// NewYAMLProviderWithExpand creates a configuration provider from a set of YAML
// file names with ${var} or $var values replaced based on the mapping function.
// Variable names not wrapped in curly braces will be parsed according
// to the shell variable naming rules:
//
//     ...a word consisting solely of underscores, digits, and
//     alphabetics from the portable character set. The first
//     character of a name is not a digit.
//
// For variables wrapped in braces, all characters between '{' and '}'
// will be passed to the expand function.  e.g. "${foo:13}" will cause
// "foo:13" to be passed to the expand function.  The sequence '$$' will
// be replaced by a literal '$'.  All other sequences will be ignored
// for expansion purposes.
func NewYAMLProviderWithExpand(mapping func(string) (string, bool), files ...string) (Provider, error) {
	readClosers, err := filesToReaders(files...)
	if err != nil {
		return nil, err
	}

	readers := make([]io.Reader, len(readClosers))
	for i, r := range readClosers {
		readers[i] = r
	}

	provider, err := NewYAMLProviderFromReaderWithExpand(mapping,
		readers...)

	for _, r := range readClosers {
		nerr := r.Close()
		if err == nil {
			err = nerr
		}
	}

	return provider, err
}

// NewYAMLProviderFromReader creates a configuration provider from a list of io.Readers.
// As above, all the objects are going to be merged and arrays/values overridden in the order of the files.
func NewYAMLProviderFromReader(readers ...io.Reader) (Provider, error) {
	p, err := newYAMLProviderCore(readers...)
	if err != nil {
		return nil, err
	}

	return newCachedProvider(p)
}

// NewYAMLProviderFromReaderWithExpand creates a configuration provider from
// a list of `io.Readers and uses the mapping function to expand values
// in the underlying provider.
func NewYAMLProviderFromReaderWithExpand(
	mapping func(string) (string, bool),
	readers ...io.Reader) (Provider, error) {

	expandFunc := replace(mapping)

	ereaders := make([]io.Reader, len(readers))
	for i, reader := range readers {
		ereaders[i] = transform.NewReader(
			reader,
			&expandTransformer{expand: expandFunc})
	}

	return NewYAMLProviderFromReader(ereaders...)
}

// NewYAMLProviderFromBytes creates a config provider from a byte-backed YAML
// blobs. As above, all the objects are going to be merged and arrays/values
// overridden in the order of the yamls.
func NewYAMLProviderFromBytes(yamls ...[]byte) (Provider, error) {
	readers := make([]io.Reader, len(yamls))
	for i, yml := range yamls {
		readers[i] = bytes.NewReader(yml)
	}

	return NewYAMLProviderFromReader(readers...)
}

func filesToReaders(files ...string) ([]io.ReadCloser, error) {
	// load the files, read their bytes
	readers := []io.ReadCloser{}

	for _, v := range files {
		if reader, err := os.Open(v); err != nil {
			for _, r := range readers {
				r.Close()
			}
			return nil, err
		} else if reader != nil {
			readers = append(readers, reader)
		}
	}

	return readers, nil
}

func (y yamlConfigProvider) getNode(key string) *yamlNode {
	if key == Root {
		return &y.root
	}

	return y.root.Find(key)
}

// Name returns the config provider name.
func (y yamlConfigProvider) Name() string {
	return "yaml"
}

// Get returns a configuration value by name
func (y yamlConfigProvider) Get(key string) Value {
	node := y.getNode(key)
	if node == nil {
		return NewValue(y, key, nil, false)
	}

	return NewValue(y, key, node.value, true)
}

// nodeType is a simple YAML reader.
type nodeType int

const (
	valueNode nodeType = iota
	objectNode
	arrayNode
)

type yamlNode struct {
	nodeType nodeType
	key      string
	value    interface{}
	children []*yamlNode
}

func (n yamlNode) String() string {
	return fmt.Sprintf("%v", n.value)
}

func (n yamlNode) Type() reflect.Type {
	return reflect.TypeOf(n.value)
}

// Find the first longest match in child nodes for the dottedPath.
func (n *yamlNode) Find(dottedPath string) *yamlNode {
	for curr := dottedPath; len(curr) != 0; {
		for _, v := range n.Children() {
			if strings.EqualFold(v.key, curr) {
				if curr == dottedPath {
					return v
				}

				if node := v.Find(dottedPath[len(curr)+1:]); node != nil {
					return node
				}
			}
		}

		if last := strings.LastIndex(curr, _separator); last > 0 {
			curr = curr[:last]
		} else {
			break
		}
	}

	return nil
}

// Children returns a slice containing this node's child nodes.
func (n *yamlNode) Children() []*yamlNode {
	if n.children == nil {
		n.children = []*yamlNode{}

		switch n.nodeType {
		case objectNode:
			for k, v := range n.value.(map[interface{}]interface{}) {
				n2 := &yamlNode{
					nodeType: getNodeType(v),
					// We need to use a default format, because key may be not a string.
					key:   fmt.Sprintf("%v", k),
					value: v,
				}

				n.children = append(n.children, n2)
			}
		case arrayNode:
			for k, v := range n.value.([]interface{}) {
				n2 := &yamlNode{
					nodeType: getNodeType(v),
					key:      strconv.Itoa(k),
					value:    v,
				}

				n.children = append(n.children, n2)
			}
		}
	}

	nodes := make([]*yamlNode, len(n.children))
	copy(nodes, n.children)
	return nodes
}

func unmarshalYAMLValue(reader io.Reader, value interface{}) error {
	raw, err := ioutil.ReadAll(reader)
	if err != nil {
		return errors.Wrap(err, "failed to read the yaml config")
	}

	return yaml.Unmarshal(raw, value)
}

// Function to expand environment variables in returned values that have form: ${ENV_VAR:DEFAULT_VALUE}.
// For example, if an HTTP_PORT environment variable should be used for the HTTP module
// port, the config would look like this:
//
//   modules:
//     http:
//       port: ${HTTP_PORT:8080}
//
// In the case that HTTP_PORT is not provided, default value (in this case 8080)
// will be used.
func replace(lookUp func(string) (string, bool)) func(in string) (string, error) {
	return func(in string) (string, error) {
		sep := strings.Index(in, _envSeparator)
		var key string
		var def string

		if sep == -1 {
			// separator missing - everything is the key ${KEY}
			key = in
		} else {
			// ${KEY:DEFAULT}
			key = in[:sep]
			def = in[sep+1:]
		}

		if envVal, ok := lookUp(key); ok {
			return envVal, nil
		}

		if def == "" {
			return "", fmt.Errorf(`default is empty for %q (use "" for empty string)`, key)
		} else if def == _emptyDefault {
			return "", nil
		}

		return def, nil
	}
}

func getNodeType(val interface{}) nodeType {
	switch val.(type) {
	case map[interface{}]interface{}:
		return objectNode
	case []interface{}:
		return arrayNode
	default:
		return valueNode
	}
}
