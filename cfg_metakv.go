//  Copyright (c) 2014 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package cbgt

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io/ioutil"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	log "github.com/couchbase/clog"

	"github.com/couchbase/cbauth/metakv"
)

// The values stored in a Cfg are normally atomic, but CfgMetaKv has
// an exception for values related to NodeDefs and PlanPIndexes.
// In CfgMetaKv, NodeDefs and PlanPIndexes are handled in "composite"
// fashion, where CfgMetaKv will split a NodeDefs/PlanPIndexes value
// into zero or more child values, each stored into metakv using a
// shared path prefix (so, similar to having a file-per-NodeDef in a
// shared directory).
//
// This maneuver helps mainly on two aspects.
// First, it allows concurrent, racing nodes to register their own entries
// into a composite NodeDefs "directory".  This can happen when
// multiple nodes are launched at nearly the same time, faster than
// metakv can replicate in eventually consistent fashion.
//
// Secondly, it helps in optimising the storage requirements of
// the PlanPIndexes at the config level by de duplicating the information.
//
// Eventually, metakv replication will occur, and a reader will see a
// union of NodeDef values that CfgMetaKv reconstructs on the fly for
// the entire "directory".

// Prefix of paths stored in metakv, and should be immutable after
// process init()'ialization.
var CfgMetaKvPrefix = "/cbgt/cfg/"

var LeanPlanVersion = "5.5.0"

// CfgAppVersion is a global Cfg variable supposed to be overridden by
// applications to indicate the current application version.
// This version information is used to figure out cluster level compatibility
// issues.
var CfgAppVersion = "6.0.0"

type cfgMetaKvAdvancedHandler interface {
	get(c *CfgMetaKv, key string, cas uint64) ([]byte, uint64, error)
	set(c *CfgMetaKv, key string, val []byte, cas uint64) (uint64, error)
	del(c *CfgMetaKv, key string, cas uint64) error
}

// PLAN_PINDEXES_KEY refers to the PlanPIndex handler which basically
// invokes the different optimised representations of the PlanPIndex
// (shared/lean) depending on the feature availability in the cluster.
//
// Lean plan re arranges the PlanPIndexes information grouped by index
// level to optimise the de duplication of information.
// So all the common fields of all the PlanPIndexes for a given index are
// stored only once per index level as given below. This boosts the savings
// with higher number of partitions per index.
// And the plans for each index are stored in sibling folders at metakv.
// Introduced since 5.1
//
// LeanPlanPIndexes -> map[indexName]
//                                  => LeanIndexPlanPIndexes
//                                     { indexDef,
//                                       map[PlanPIndexName]=> LeanPlanPIndex{}
//                                     }
//
// Shared plan refers to another approach for saving space while
// storing the PlanPIndexes. Here common fields like IndexParams and
// SourceParams across all PlanPIndexes of an index are deduplicated.
// And the entire PlanPIndexes are stored under the same folder at metakv.

var cfgMetaKvAdvancedKeys map[string]cfgMetaKvAdvancedHandler = map[string]cfgMetaKvAdvancedHandler{
	CfgNodeDefsKey(NODE_DEFS_WANTED): &cfgMetaKvNodeDefsSplitHandler{},
	CfgNodeDefsKey(NODE_DEFS_KNOWN):  &cfgMetaKvNodeDefsSplitHandler{},
	PLAN_PINDEXES_KEY:                &cfgMetaKvPlanPIndexesHandler{},
}

type CfgMetaKv struct {
	prefix   string // Prefix for paths stores in metakv.
	nodeUUID string // The uuid for this node.

	m            sync.Mutex // Protects the fields that follow.
	cfgMem       *CfgMem
	cancelCh     chan struct{}
	lastSplitCAS uint64
	splitEntries map[string]CfgMetaKvEntry
	nsServerUrl  string
}

type CfgMetaKvEntry struct {
	cas  uint64
	data []byte
}

// VersionReader is an interface to be implemented by the
// configuration providers who supports the verification of
// homogeneousness of the cluster before performing certain
// Key/Values updates related to the cluster status
type VersionReader interface {
	// ClusterVersion retrieves the cluster
	// compatibility information from the ns_server
	ClusterVersion() (uint64, error)
}

// NewCfgMetaKv returns a CfgMetaKv that reads and stores its single
// configuration file in the metakv.
func NewCfgMetaKv(nodeUUID string, options map[string]string) (*CfgMetaKv, error) {
	nsServerURL, _ := options["nsServerURL"]

	cfg := &CfgMetaKv{
		prefix:       CfgMetaKvPrefix,
		nodeUUID:     nodeUUID,
		cfgMem:       NewCfgMem(),
		cancelCh:     make(chan struct{}),
		splitEntries: map[string]CfgMetaKvEntry{},
	}

	backoffStartSleepMS := 200
	backoffFactor := float32(1.5)
	backoffMaxSleepMS := 5000
	leanPlanKeyPrefix = CfgMetaKvPrefix + leanPlanKeyPrefix

	cfg.nsServerUrl = nsServerURL + "/pools/default"

	go ExponentialBackoffLoop("cfg_metakv.RunObserveChildren",
		func() int {
			err := metakv.RunObserveChildren(cfg.prefix, cfg.metaKVCallback,
				cfg.cancelCh)
			if err == nil {
				return -1 // Success, so stop the loop.
			}

			log.Printf("cfg_metakv: RunObserveChildren, err: %v", err)

			return 0 // No progress, so exponential backoff.
		},
		backoffStartSleepMS, backoffFactor, backoffMaxSleepMS)

	return cfg, nil
}

func (c *CfgMetaKv) Get(key string, cas uint64) ([]byte, uint64, error) {
	c.m.Lock()
	data, cas, err := c.getLOCKED(key, cas)
	c.m.Unlock()

	return data, cas, err
}

func (c *CfgMetaKv) getLOCKED(key string, cas uint64) ([]byte, uint64, error) {
	handler := cfgMetaKvAdvancedKeys[key]
	if handler != nil {
		return handler.get(c, key, cas)
	}

	return c.getRawLOCKED(key, cas)
}

func (c *CfgMetaKv) getRawLOCKED(key string, cas uint64) ([]byte, uint64, error) {
	path := c.keyToPath(key)

	v, _, err := metakv.Get(path) // TODO: Handle rev.
	if err != nil {
		return nil, 0, err
	}

	return v, 1, nil
}

func (c *CfgMetaKv) Set(key string, val []byte, cas uint64) (
	uint64, error) {
	log.Printf("cfg_metakv: Set, key: %v, cas: %x, split: %t, nodeUUID: %s",
		key, cas, cfgMetaKvAdvancedKeys[key] != nil, c.nodeUUID)

	c.m.Lock()
	casResult, err := c.setLOCKED(key, val, cas)
	c.m.Unlock()

	return casResult, err
}

func (c *CfgMetaKv) setLOCKED(key string, val []byte, cas uint64) (
	uint64, error) {
	handler := cfgMetaKvAdvancedKeys[key]
	if handler != nil {
		return handler.set(c, key, val, cas)
	}

	return c.setRawLOCKED(key, val, cas)
}

func (c *CfgMetaKv) setRawLOCKED(key string, val []byte, cas uint64) (
	uint64, error) {
	path := c.keyToPath(key)

	log.Printf("cfg_metakv: Set path: %v", path)

	err := metakv.Set(path, val, nil) // TODO: Handle rev better.
	if err != nil {
		return 0, err
	}

	return 1, nil
}

func (c *CfgMetaKv) Del(key string, cas uint64) error {
	c.m.Lock()
	err := c.delLOCKED(key, cas)
	c.m.Unlock()

	return err
}

func (c *CfgMetaKv) delLOCKED(key string, cas uint64) error {
	handler := cfgMetaKvAdvancedKeys[key]
	if handler != nil {
		return handler.del(c, key, cas)
	}

	return c.delRawLOCKED(key, cas)
}

func (c *CfgMetaKv) delRawLOCKED(key string, cas uint64) error {
	path := c.keyToPath(key)

	return metakv.Delete(path, nil) // TODO: Handle rev better.
}

func (c *CfgMetaKv) Load() error {
	metakv.IterateChildren(c.prefix, c.metaKVCallback)

	return nil
}

func (c *CfgMetaKv) metaKVCallback(path string,
	value []byte, rev interface{}) error {
	key := c.pathToKey(path)

	log.Printf("cfg_metakv: metaKVCallback, path: %v, key: %v,"+
		" deletion: %t", path, key, value == nil)

	for splitKeyPrefix := range cfgMetaKvAdvancedKeys {
		// Handle the case when the key from metakv looks like
		// "nodeDefs-known/63f8a79660", but the subscription was for a
		// key prefix like "nodeDefs-known".
		if strings.HasPrefix(key, splitKeyPrefix) {
			key = splitKeyPrefix
		}
	}

	c.m.Lock()
	c.cfgMem.FireEvent(key, 1, nil)
	c.m.Unlock()

	return nil
}

func (c *CfgMetaKv) Subscribe(key string, ch chan CfgEvent) error {
	c.m.Lock()
	err := c.cfgMem.Subscribe(key, ch)
	c.m.Unlock()

	return err
}

func (c *CfgMetaKv) Refresh() error {
	return c.Load()
}

// RemoveAllKeys removes all cfg entries from metakv, where the caller
// should no longer use this CfgMetaKv instance, but instead create a
// new instance.
func (c *CfgMetaKv) RemoveAllKeys() {
	metakv.RecursiveDelete(c.prefix)
}

func (c *CfgMetaKv) keyToPath(key string) string {
	return c.prefix + key
}

func (c *CfgMetaKv) pathToKey(k string) string {
	return k[len(c.prefix):]
}

// for testing
func (c *CfgMetaKv) listChildPaths(key string) ([]string, error) {
	g := []string{}

	if cfgMetaKvAdvancedKeys[key] != nil {
		m, err := metakv.ListAllChildren(c.keyToPath(key) + "/")
		if err != nil {
			return nil, err
		}
		for _, v := range m {
			g = append(g, v.Path)
		}
	}

	return g, nil
}

func checkSumUUIDs(uuids []string) string {
	sort.Strings(uuids)
	d, _ := json.Marshal(uuids)
	return fmt.Sprint(crc32.ChecksumIEEE(d))
}

// ----------------------------------------------------------------

type cfgMetaKvNodeDefsSplitHandler struct{}

// get() retrieves multiple child entries from the metakv and weaves
// the results back into a composite nodeDefs.  get() must be invoked
// with c.m.Lock()'ed.
func (a *cfgMetaKvNodeDefsSplitHandler) get(
	c *CfgMetaKv, key string, cas uint64) ([]byte, uint64, error) {
	m, err := metakv.ListAllChildren(c.keyToPath(key) + "/")
	if err != nil {
		return nil, 0, err
	}

	rv := &NodeDefs{NodeDefs: make(map[string]*NodeDef)}

	uuids := []string{}
	for _, v := range m {
		var childNodeDefs NodeDefs

		err = json.Unmarshal(v.Value, &childNodeDefs)
		if err != nil {
			return nil, 0, err
		}

		for k1, v1 := range childNodeDefs.NodeDefs {
			rv.NodeDefs[k1] = v1
		}

		if rv.ImplVersion == "" ||
			VersionGTE(childNodeDefs.ImplVersion, rv.ImplVersion) {
			rv.ImplVersion = childNodeDefs.ImplVersion
		}

		uuids = append(uuids, childNodeDefs.UUID)
	}
	rv.UUID = checkSumUUIDs(uuids)

	if rv.ImplVersion == "" {
		rv.ImplVersion = VERSION
	}

	data, err := json.Marshal(rv)
	if err != nil {
		return nil, 0, err
	}

	casResult := c.lastSplitCAS + 1
	c.lastSplitCAS = casResult

	c.splitEntries[key] = CfgMetaKvEntry{
		cas:  casResult,
		data: data,
	}

	return data, casResult, nil
}

// set() splits a nodeDefs into multiple child metakv entries and must
// be invoked with c.m.Lock()'ed.
func (a *cfgMetaKvNodeDefsSplitHandler) set(
	c *CfgMetaKv, key string, val []byte, cas uint64) (uint64, error) {
	path := c.keyToPath(key)

	curEntry := c.splitEntries[key]

	if cas != math.MaxUint64 && cas != 0 && cas != curEntry.cas {
		log.Warnf("cfg_metakv: Set split, key: %v, cas mismatch: %x != %x",
			key, cas, curEntry.cas)

		return 0, &CfgCASError{}
	}

	var curNodeDefs NodeDefs

	if curEntry.data != nil && len(curEntry.data) > 0 {
		err := json.Unmarshal(curEntry.data, &curNodeDefs)
		if err != nil {
			return 0, err
		}
	}

	var nd NodeDefs

	err := json.Unmarshal(val, &nd)
	if err != nil {
		return 0, err
	}

	// Analyze which children were added, removed, updated.
	//
	added := map[string]bool{}
	removed := map[string]bool{}
	updated := map[string]bool{}

	for k, v := range nd.NodeDefs {
		if curNodeDefs.NodeDefs == nil ||
			curNodeDefs.NodeDefs[k] == nil {
			added[k] = true
		} else {
			if !reflect.DeepEqual(curNodeDefs.NodeDefs[k], v) {
				updated[k] = true
			}
		}
	}

	if curNodeDefs.NodeDefs != nil {
		for k := range curNodeDefs.NodeDefs {
			if nd.NodeDefs[k] == nil {
				removed[k] = true
			}
		}
	}

	log.Printf("cfg_metakv: Set split, key: %v,"+
		" added: %v, removed: %v, updated: %v",
		key, added, removed, updated)

LOOP:
	for k, v := range nd.NodeDefs {
		if cas != math.MaxUint64 && c.nodeUUID != "" && c.nodeUUID != v.UUID {
			// If we have a nodeUUID, only add/update our
			// nodeDef, where other nodes will each add/update
			// only their own nodeDef's.
			log.Printf("cfg_metakv: Set split, key: %v,"+
				" skipping other node UUID: %v, self nodeUUID: %s",
				key, v.UUID, c.nodeUUID)

			continue LOOP
		}

		childNodeDefs := NodeDefs{
			UUID:        nd.UUID,
			NodeDefs:    map[string]*NodeDef{},
			ImplVersion: nd.ImplVersion,
		}
		childNodeDefs.NodeDefs[k] = v

		val, err = json.Marshal(childNodeDefs)
		if err != nil {
			return 0, err
		}

		childPath := path + "/" + k

		log.Printf("cfg_metakv: Set split, key: %v, childPath: %v",
			key, childPath)

		err = metakv.Set(childPath, val, nil)
		if err != nil {
			return 0, err
		}
		if cas != math.MaxUint64 {
			break LOOP
		}
	}

	// Remove composite children entries from metakv only if
	// caller was attempting removals only.  This should work as
	// the caller usually has read-compute-write logic that only
	// removes node defs and does not add/update node defs in the
	// same read-compute-write code path.
	//
	if len(added) <= 0 && len(updated) <= 0 && len(removed) > 0 {
		for nodeDefUUID := range removed {
			childPath := path + "/" + nodeDefUUID

			log.Printf("cfg_metakv: Set delete, childPath: %v", childPath)

			err = metakv.Delete(childPath, nil)
			if err != nil {
				return 0, err
			}
		}
	}

	casResult := c.lastSplitCAS + 1
	c.lastSplitCAS = casResult
	return casResult, err
}

func (a *cfgMetaKvNodeDefsSplitHandler) del(
	c *CfgMetaKv, key string, cas uint64) error {
	delete(c.splitEntries, key)

	path := c.keyToPath(key)

	return metakv.RecursiveDelete(path + "/")
}

// ----------------------------------------------------------------

// cfgMetaKvPlanPIndexesHandler invokes the appropriate plan handler
// depending on the feature availability
type cfgMetaKvPlanPIndexesHandler struct{}

func (a *cfgMetaKvPlanPIndexesHandler) get(c *CfgMetaKv,
	key string, cas uint64) ([]byte, uint64, error) {
	// if the cluster contains any nodes which dont support
	// the Lean planPIndex feature then fallback to older
	// shared PlanPIndex format
	if isLeanPlanSupported(c) {
		return getLeanPlan(c, key, cas)
	}
	return getSharedPlan(c, key, cas)
}

func (a *cfgMetaKvPlanPIndexesHandler) set(c *CfgMetaKv,
	key string, val []byte, cas uint64) (uint64, error) {
	// if the cluster contains any nodes which dont support
	// the feature Lean planPIndex feature then fallback to older
	// shared PlanPIndex format
	if isLeanPlanSupported(c) {
		return setLeanPlan(c, key, val, cas)
	}
	return setSharedPlan(c, key, val, cas)
}

func (a *cfgMetaKvPlanPIndexesHandler) del(c *CfgMetaKv,
	key string, cas uint64) error {
	// if the cluster contains any nodes which dont support
	// the feature Lean planPIndex feature then fallback to older
	// shared PlanPIndex format
	if isLeanPlanSupported(c) {
		return delLeanPlan(c, key, cas)
	}
	return delSharedPlan(c, key, cas)
}

func isLeanPlanSupported(c *CfgMetaKv) bool {
	b, _, err := c.getRawLOCKED(VERSION_KEY, 0)
	if err != nil {
		return false
	}
	// if the Cfg version is lower than 5.5.0/LeanPlanVersion
	// then return false
	if VersionGTE(string(b), LeanPlanVersion) == false {
		return false
	}

	// extra check to see if the cluster contains any nodes which
	// dont support the Lean planPIndex feature then fallback to
	// older shared PlanPIndex format
	for _, k := range []string{NODE_DEFS_KNOWN, NODE_DEFS_WANTED} {
		key := CfgNodeDefsKey(k)
		handler := cfgMetaKvAdvancedKeys[key]
		if handler == nil {
			return false
		}

		v, _, err := handler.get(c, key, 0)
		if err != nil {
			log.Printf("cfg_metakv: isLeanPlanSupported, err: %v", err)
			return false
		}
		if v == nil {
			return false
		}
		rv := &NodeDefs{}
		err = json.Unmarshal(v, rv)
		if err != nil {
			log.Printf("cfg_metakv: isLeanPlanSupported, json unmarshal, err: %v", err)
			return false
		}

		if !IsFeatureSupportedByCluster(NodeFeatureLeanPlan, rv) {
			return false
		}
	}
	return true
}

// PlanPIndexesShared represents a PlanPIndexes that has been
// deduplicated into shared parts.
type PlanPIndexesShared struct {
	PlanPIndexes

	// Key is "indexType/indexName/indexUUID".
	SharedIndexDefs map[string]*PlanPIndexIndexDef `json:"sharedIndexDefs"`

	// Key is "sourceType/sourceName/sourceUUID".
	SharedSourceDefs map[string]*PlanPIndexSourceDef `json:"sharedSourceDefs"`
}

// PlanPIndexIndexDef represents the shared, repeated index definition
// part of a PlanPIndex.
type PlanPIndexIndexDef struct {
	IndexParams string `json:"indexParams"`
}

// PlanPIndexSourceDef represents the shared, repeated source
// definition part of a PlanPIndex.
type PlanPIndexSourceDef struct {
	SourceParams string `json:"sourceParams"`
}

func getSharedPlan(c *CfgMetaKv,
	key string, cas uint64) ([]byte, uint64, error) {
	buf, casResult, err := c.getRawLOCKED(key, cas)
	if err != nil || len(buf) <= 0 {
		return buf, casResult, err
	}

	var shared PlanPIndexesShared

	err = json.Unmarshal(buf, &shared)
	if err != nil {
		return nil, 0, err
	}

	planPIndexes := &shared.PlanPIndexes

	// Expand/reconstitute the planPIndex's name.
	for ppiName, ppi := range planPIndexes.PlanPIndexes {
		if ppi.Name == "" {
			ppi.Name = ppiName
		}
	}

	// Expand/reconstitute the index def part of each planPIndex.
	if shared.SharedIndexDefs != nil {
		for _, ppi := range planPIndexes.PlanPIndexes {
			if ppi.IndexParams == "" {
				k := ppi.IndexType + "/" + ppi.IndexName + "/" + ppi.IndexUUID
				sharedPart, ok := shared.SharedIndexDefs[k]
				if ok && sharedPart != nil {
					ppi.IndexParams = sharedPart.IndexParams
				}
			}
		}
	}

	// Expand/reconstitute the source def part of each planPIndex.
	if shared.SharedSourceDefs != nil {
		for _, ppi := range planPIndexes.PlanPIndexes {
			if ppi.SourceParams == "" {
				k := ppi.SourceType + "/" + ppi.SourceName + "/" + ppi.SourceUUID +
					"/" + ppi.IndexName + "/" + ppi.IndexUUID
				sharedPart, ok := shared.SharedSourceDefs[k]
				if ok && sharedPart != nil {
					ppi.SourceParams = sharedPart.SourceParams
				}
			}
		}
	}

	bufResult, err := json.Marshal(planPIndexes)
	if err != nil {
		return nil, 0, err
	}

	return bufResult, casResult, nil
}

// setSharedPlan rewrites the set of a planPIndex
// document by deduplicating repeated index definitions and source
// definitions.
func setSharedPlan(c *CfgMetaKv,
	key string, val []byte, cas uint64) (uint64, error) {
	var shared PlanPIndexesShared

	err := json.Unmarshal(val, &shared.PlanPIndexes)
	if err != nil {
		return 0, err
	}

	shared.SharedIndexDefs = map[string]*PlanPIndexIndexDef{}
	shared.SharedSourceDefs = map[string]*PlanPIndexSourceDef{}

	// Reduce the planPIndexes by not repeating the shared parts.
	for _, ppi := range shared.PlanPIndexes.PlanPIndexes {
		ppi.Name = ""

		if ppi.IndexParams != "" {
			k := ppi.IndexType + "/" + ppi.IndexName + "/" + ppi.IndexUUID
			shared.SharedIndexDefs[k] = &PlanPIndexIndexDef{
				IndexParams: ppi.IndexParams,
			}
			ppi.IndexParams = ""
		}

		if ppi.SourceParams != "" {
			k := ppi.SourceType + "/" + ppi.SourceName + "/" + ppi.SourceUUID +
				"/" + ppi.IndexName + "/" + ppi.IndexUUID
			shared.SharedSourceDefs[k] = &PlanPIndexSourceDef{
				SourceParams: ppi.SourceParams,
			}
			ppi.SourceParams = ""
		}
	}

	valShared, err := json.Marshal(&shared)
	if err != nil {
		return 0, err
	}

	return c.setRawLOCKED(key, valShared, cas)
}

func delSharedPlan(c *CfgMetaKv, key string, cas uint64) error {
	return c.delRawLOCKED(key, cas)
}

type Compatibility struct {
	ClusterCompatibility int `json:"clusterCompatibility"`
}

type NsServerResponse struct {
	Nodes []Compatibility `json:"nodes"`
}

func (c *CfgMetaKv) ClusterVersion() (uint64, error) {
	if len(c.nsServerUrl) < 2 {
		return 1, fmt.Errorf("cfg_metakv: no ns_server URL configured")
	}

	u, err := CBAuthURL(c.nsServerUrl)
	if err != nil {
		return 0, fmt.Errorf("cfg_metakv: auth for ns_server,"+
			" nsServerURL: %s, authType: %s, err: %v",
			c.nsServerUrl, "cbauth", err)
	}

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, err
	}

	req.Header.Add("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	respBuf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("cfg_metakv: error reading resp.Body,"+
			" nsServerURL: %s, resp: %#v, err: %v", c.nsServerUrl, resp, err)
	}

	rv := &NsServerResponse{}
	err = json.Unmarshal(respBuf, rv)
	if err != nil {
		return 0, fmt.Errorf("cfg_metakv: error parsing respBuf: %s,"+
			" nsServerURL: %s, err: %v", respBuf, c.nsServerUrl, err)

	}

	return uint64(rv.Nodes[0].ClusterCompatibility), nil
}

func CompatibilityVersion(version string) (uint64, error) {
	eVersion := uint64(1)
	xa := strings.Split(version, ".")
	if len(xa) < 2 {
		return eVersion, fmt.Errorf("invalid version")
	}

	majVersion, err := strconv.Atoi(xa[0])
	if err != nil {
		return eVersion, err
	}

	minVersion, err := strconv.Atoi(xa[1])
	if err != nil {
		return eVersion, err
	}

	return uint64(65536*majVersion + minVersion), nil
}
