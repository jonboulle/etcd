/*
Copyright 2013 CoreOS Inc.

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

package store

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	etcdErr "github.com/coreos/etcd/error"
)

// The default version to set when the store is first initialized.
const defaultVersion = 2

var minExpireTime time.Time

func init() {
	minExpireTime, _ = time.Parse(time.RFC3339, "2000-01-01T00:00:00Z")
}

type Store interface {
	Version() int
	Index() uint64

	Get(nodePath string, recursive, sorted bool) (*Event, error)
	Set(nodePath string, dir bool, value string, expireTime time.Time) (*Event, error)
	Update(nodePath string, newValue string, expireTime time.Time) (*Event, error)
	Create(nodePath string, dir bool, value string, unique bool,
		expireTime time.Time) (*Event, error)
	CompareAndSwap(nodePath string, prevValue string, prevIndex uint64,
		value string, expireTime time.Time) (*Event, error)
	Delete(nodePath string, recursive, dir bool) (*Event, error)
	CompareAndDelete(nodePath string, prevValue string, prevIndex uint64) (*Event, error)

	Watch(prefix string, recursive, stream bool, sinceIndex uint64) (Watcher, error)

	Save() ([]byte, error)
	Recovery(state []byte) error

	TotalTransactions() uint64
	JsonStats() []byte
	DeleteExpiredKeys(cutoff time.Time)
}

type store struct {
	watcherHub     *watcherHub
	root           *node
	currentIndex   uint64
	stats          *Stats
	currentVersion int
	ttlKeyHeap     *ttlKeyHeap  // need to recovery manually
	worldLock      sync.RWMutex // stop the world lock
}

func New() Store {
	return newStore()
}

func newStore() *store {
	s := new(store)
	s.currentVersion = defaultVersion
	s.root = newDir(s, "/", s.currentIndex, nil, "", Permanent)
	s.stats = newStats()
	s.watcherHub = newWatchHub(1000)
	s.ttlKeyHeap = newTtlKeyHeap()
	return s
}

// Version retrieves current version of the store.
func (s *store) Version() int {
	return s.currentVersion
}

// Retrieves current of the store
func (s *store) Index() uint64 {
	s.worldLock.RLock()
	defer s.worldLock.RUnlock()
	return s.currentIndex
}

// Get returns a get event.
// If recursive is true, it will return all the content under the node path.
// If sorted is true, it will sort the content by keys.
func (s *store) Get(nodePath string, recursive, sorted bool) (*Event, error) {
	s.worldLock.RLock()
	defer s.worldLock.RUnlock()

	nodePath = path.Clean(path.Join("/", nodePath))

	n, err := s.internalGet(nodePath)

	if err != nil {
		s.stats.Inc(GetFail)
		return nil, err
	}

	e := newEvent(Get, nodePath, n.ModifiedIndex, n.CreatedIndex)
	e.Node.loadInternalNode(n, recursive, sorted)

	s.stats.Inc(GetSuccess)

	return e, nil
}

// Create creates the node at nodePath. Create will help to create intermediate directories with no ttl.
// If the node has already existed, create will fail.
// If any node on the path is a file, create will fail.
func (s *store) Create(nodePath string, dir bool, value string, unique bool, expireTime time.Time) (*Event, error) {
	s.worldLock.Lock()
	defer s.worldLock.Unlock()
	e, err := s.internalCreate(nodePath, dir, value, unique, false, expireTime, Create)

	if err == nil {
		s.watcherHub.notify(e)
		s.stats.Inc(CreateSuccess)
	} else {
		s.stats.Inc(CreateFail)
	}

	return e, err
}

// Set creates or replace the node at nodePath.
func (s *store) Set(nodePath string, dir bool, value string, expireTime time.Time) (*Event, error) {
	var err error

	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	defer func() {
		if err == nil {
			s.stats.Inc(SetSuccess)
		} else {
			s.stats.Inc(SetFail)
		}
	}()

	// Get prevNode value
	n, getErr := s.internalGet(nodePath)
	if getErr != nil && getErr.ErrorCode != etcdErr.EcodeKeyNotFound {
		err = getErr
		return nil, err
	}

	// Set new value
	e, err := s.internalCreate(nodePath, dir, value, false, true, expireTime, Set)
	if err != nil {
		return nil, err
	}

	// Put prevNode into event
	if getErr == nil {
		prev := newEvent(Get, nodePath, n.ModifiedIndex, n.CreatedIndex)
		prev.Node.loadInternalNode(n, false, false)
		e.PrevNode = prev.Node
	}

	s.watcherHub.notify(e)

	return e, nil
}

// returns user-readable cause of failed comparison
func getCompareFailCause(n *node, which int, prevValue string, prevIndex uint64) string {
	switch which {
	case CompareIndexNotMatch:
		return fmt.Sprintf("[%v != %v]", prevIndex, n.ModifiedIndex)
	case CompareValueNotMatch:
		return fmt.Sprintf("[%v != %v]", prevValue, n.Value)
	default:
		return fmt.Sprintf("[%v != %v] [%v != %v]", prevValue, n.Value, prevIndex, n.ModifiedIndex)
	}
}

func (s *store) CompareAndSwap(nodePath string, prevValue string, prevIndex uint64,
	value string, expireTime time.Time) (*Event, error) {

	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	nodePath = path.Clean(path.Join("/", nodePath))
	// we do not allow the user to change "/"
	if nodePath == "/" {
		return nil, etcdErr.NewError(etcdErr.EcodeRootROnly, "/", s.currentIndex)
	}

	n, err := s.internalGet(nodePath)

	if err != nil {
		s.stats.Inc(CompareAndSwapFail)
		return nil, err
	}

	if n.IsDir() { // can only compare and swap file
		s.stats.Inc(CompareAndSwapFail)
		return nil, etcdErr.NewError(etcdErr.EcodeNotFile, nodePath, s.currentIndex)
	}

	// If both of the prevValue and prevIndex are given, we will test both of them.
	// Command will be executed, only if both of the tests are successful.
	if ok, which := n.Compare(prevValue, prevIndex); !ok {
		cause := getCompareFailCause(n, which, prevValue, prevIndex)
		s.stats.Inc(CompareAndSwapFail)
		return nil, etcdErr.NewError(etcdErr.EcodeTestFailed, cause, s.currentIndex)
	}

	// update etcd index
	s.currentIndex++

	e := newEvent(CompareAndSwap, nodePath, s.currentIndex, n.CreatedIndex)
	e.PrevNode = n.Repr(false, false)
	eNode := e.Node

	// if test succeed, write the value
	n.Write(value, s.currentIndex)
	n.UpdateTTL(expireTime)

	// copy the value for safety
	valueCopy := value
	eNode.Value = &valueCopy
	eNode.Expiration, eNode.TTL = n.ExpirationAndTTL()

	s.watcherHub.notify(e)
	s.stats.Inc(CompareAndSwapSuccess)
	return e, nil
}

// Delete deletes the node at the given path.
// If the node is a directory, recursive must be true to delete it.
func (s *store) Delete(nodePath string, dir, recursive bool) (*Event, error) {
	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	nodePath = path.Clean(path.Join("/", nodePath))
	// we do not allow the user to change "/"
	if nodePath == "/" {
		return nil, etcdErr.NewError(etcdErr.EcodeRootROnly, "/", s.currentIndex)
	}

	// recursive implies dir
	if recursive == true {
		dir = true
	}

	n, err := s.internalGet(nodePath)

	if err != nil { // if the node does not exist, return error
		s.stats.Inc(DeleteFail)
		return nil, err
	}

	nextIndex := s.currentIndex + 1
	e := newEvent(Delete, nodePath, nextIndex, n.CreatedIndex)
	e.PrevNode = n.Repr(false, false)
	eNode := e.Node

	if n.IsDir() {
		eNode.Dir = true
	}

	callback := func(path string) { // notify function
		// notify the watchers with deleted set true
		s.watcherHub.notifyWatchers(e, path, true)
	}

	err = n.Remove(dir, recursive, callback)

	if err != nil {
		s.stats.Inc(DeleteFail)
		return nil, err
	}

	// update etcd index
	s.currentIndex++

	s.watcherHub.notify(e)

	s.stats.Inc(DeleteSuccess)

	return e, nil
}

func (s *store) CompareAndDelete(nodePath string, prevValue string, prevIndex uint64) (*Event, error) {
	nodePath = path.Clean(path.Join("/", nodePath))

	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	n, err := s.internalGet(nodePath)

	if err != nil { // if the node does not exist, return error
		s.stats.Inc(CompareAndDeleteFail)
		return nil, err
	}

	if n.IsDir() { // can only compare and delete file
		s.stats.Inc(CompareAndSwapFail)
		return nil, etcdErr.NewError(etcdErr.EcodeNotFile, nodePath, s.currentIndex)
	}

	// If both of the prevValue and prevIndex are given, we will test both of them.
	// Command will be executed, only if both of the tests are successful.
	if ok, which := n.Compare(prevValue, prevIndex); !ok {
		cause := getCompareFailCause(n, which, prevValue, prevIndex)
		s.stats.Inc(CompareAndDeleteFail)
		return nil, etcdErr.NewError(etcdErr.EcodeTestFailed, cause, s.currentIndex)
	}

	// update etcd index
	s.currentIndex++

	e := newEvent(CompareAndDelete, nodePath, s.currentIndex, n.CreatedIndex)
	e.PrevNode = n.Repr(false, false)

	callback := func(path string) { // notify function
		// notify the watchers with deleted set true
		s.watcherHub.notifyWatchers(e, path, true)
	}

	// delete a key-value pair, no error should happen
	n.Remove(false, false, callback)

	s.watcherHub.notify(e)
	s.stats.Inc(CompareAndDeleteSuccess)
	return e, nil
}

func (s *store) Watch(key string, recursive, stream bool, sinceIndex uint64) (Watcher, error) {
	s.worldLock.RLock()
	defer s.worldLock.RUnlock()

	key = path.Clean(path.Join("/", key))
	nextIndex := s.currentIndex + 1

	var w Watcher
	var err *etcdErr.Error

	if sinceIndex == 0 {
		w, err = s.watcherHub.watch(key, recursive, stream, nextIndex)

	} else {
		w, err = s.watcherHub.watch(key, recursive, stream, sinceIndex)
	}

	if err != nil {
		// watchhub do not know the current Index
		// we need to attach the currentIndex here
		err.Index = s.currentIndex
		return nil, err
	}

	return w, nil
}

// walk walks all the nodePath and apply the walkFunc on each directory
func (s *store) walk(nodePath string, walkFunc func(prev *node, component string) (*node, *etcdErr.Error)) (*node, *etcdErr.Error) {
	components := strings.Split(nodePath, "/")

	curr := s.root
	var err *etcdErr.Error

	for i := 1; i < len(components); i++ {
		if len(components[i]) == 0 { // ignore empty string
			return curr, nil
		}

		curr, err = walkFunc(curr, components[i])
		if err != nil {
			return nil, err
		}

	}

	return curr, nil
}

// Update updates the value/ttl of the node.
// If the node is a file, the value and the ttl can be updated.
// If the node is a directory, only the ttl can be updated.
func (s *store) Update(nodePath string, newValue string, expireTime time.Time) (*Event, error) {
	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	nodePath = path.Clean(path.Join("/", nodePath))
	// we do not allow the user to change "/"
	if nodePath == "/" {
		return nil, etcdErr.NewError(etcdErr.EcodeRootROnly, "/", s.currentIndex)
	}

	currIndex, nextIndex := s.currentIndex, s.currentIndex+1

	n, err := s.internalGet(nodePath)

	if err != nil { // if the node does not exist, return error
		s.stats.Inc(UpdateFail)
		return nil, err
	}

	e := newEvent(Update, nodePath, nextIndex, n.CreatedIndex)
	e.PrevNode = n.Repr(false, false)
	eNode := e.Node

	if n.IsDir() && len(newValue) != 0 {
		// if the node is a directory, we cannot update value to non-empty
		s.stats.Inc(UpdateFail)
		return nil, etcdErr.NewError(etcdErr.EcodeNotFile, nodePath, currIndex)
	}

	n.Write(newValue, nextIndex)

	if n.IsDir() {
		eNode.Dir = true
	} else {
		// copy the value for safety
		newValueCopy := newValue
		eNode.Value = &newValueCopy
	}

	// update ttl
	n.UpdateTTL(expireTime)

	eNode.Expiration, eNode.TTL = n.ExpirationAndTTL()

	s.watcherHub.notify(e)

	s.stats.Inc(UpdateSuccess)

	s.currentIndex = nextIndex

	return e, nil
}

func (s *store) internalCreate(nodePath string, dir bool, value string, unique, replace bool,
	expireTime time.Time, action string) (*Event, error) {

	currIndex, nextIndex := s.currentIndex, s.currentIndex+1

	if unique { // append unique item under the node path
		nodePath += "/" + strconv.FormatUint(nextIndex, 10)
	}

	nodePath = path.Clean(path.Join("/", nodePath))

	// we do not allow the user to change "/"
	if nodePath == "/" {
		return nil, etcdErr.NewError(etcdErr.EcodeRootROnly, "/", currIndex)
	}

	// Assume expire times that are way in the past are not valid.
	// This can occur when the time is serialized to JSON and read back in.
	if expireTime.Before(minExpireTime) {
		expireTime = Permanent
	}

	dirName, nodeName := path.Split(nodePath)

	// walk through the nodePath, create dirs and get the last directory node
	d, err := s.walk(dirName, s.checkDir)

	if err != nil {
		s.stats.Inc(SetFail)
		err.Index = currIndex
		return nil, err
	}

	e := newEvent(action, nodePath, nextIndex, nextIndex)
	eNode := e.Node

	n, _ := d.GetChild(nodeName)

	// force will try to replace a existing file
	if n != nil {
		if replace {
			if n.IsDir() {
				return nil, etcdErr.NewError(etcdErr.EcodeNotFile, nodePath, currIndex)
			}
			e.PrevNode = n.Repr(false, false)

			n.Remove(false, false, nil)
		} else {
			return nil, etcdErr.NewError(etcdErr.EcodeNodeExist, nodePath, currIndex)
		}
	}

	if !dir { // create file
		// copy the value for safety
		valueCopy := value
		eNode.Value = &valueCopy

		n = newKV(s, nodePath, value, nextIndex, d, "", expireTime)

	} else { // create directory
		eNode.Dir = true

		n = newDir(s, nodePath, nextIndex, d, "", expireTime)
	}

	// we are sure d is a directory and does not have the children with name n.Name
	d.Add(n)

	// node with TTL
	if !n.IsPermanent() {
		s.ttlKeyHeap.push(n)

		eNode.Expiration, eNode.TTL = n.ExpirationAndTTL()
	}

	s.currentIndex = nextIndex

	return e, nil
}

// InternalGet gets the node of the given nodePath.
func (s *store) internalGet(nodePath string) (*node, *etcdErr.Error) {
	nodePath = path.Clean(path.Join("/", nodePath))

	walkFunc := func(parent *node, name string) (*node, *etcdErr.Error) {

		if !parent.IsDir() {
			err := etcdErr.NewError(etcdErr.EcodeNotDir, parent.Path, s.currentIndex)
			return nil, err
		}

		child, ok := parent.Children[name]
		if ok {
			return child, nil
		}

		return nil, etcdErr.NewError(etcdErr.EcodeKeyNotFound, path.Join(parent.Path, name), s.currentIndex)
	}

	f, err := s.walk(nodePath, walkFunc)

	if err != nil {
		return nil, err
	}
	return f, nil
}

// deleteExpiredKyes will delete all
func (s *store) DeleteExpiredKeys(cutoff time.Time) {
	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	for {
		node := s.ttlKeyHeap.top()
		if node == nil || node.ExpireTime.After(cutoff) {
			break
		}

		s.currentIndex++
		e := newEvent(Expire, node.Path, s.currentIndex, node.CreatedIndex)
		e.PrevNode = node.Repr(false, false)

		callback := func(path string) { // notify function
			// notify the watchers with deleted set true
			s.watcherHub.notifyWatchers(e, path, true)
		}

		s.ttlKeyHeap.pop()
		node.Remove(true, true, callback)

		s.stats.Inc(ExpireCount)

		s.watcherHub.notify(e)
	}

}

// checkDir will check whether the component is a directory under parent node.
// If it is a directory, this function will return the pointer to that node.
// If it does not exist, this function will create a new directory and return the pointer to that node.
// If it is a file, this function will return error.
func (s *store) checkDir(parent *node, dirName string) (*node, *etcdErr.Error) {
	node, ok := parent.Children[dirName]

	if ok {
		if node.IsDir() {
			return node, nil
		}

		return nil, etcdErr.NewError(etcdErr.EcodeNotDir, node.Path, s.currentIndex)
	}

	n := newDir(s, path.Join(parent.Path, dirName), s.currentIndex+1, parent, parent.ACL, Permanent)

	parent.Children[dirName] = n

	return n, nil
}

// Save saves the static state of the store system.
// It will not be able to save the state of watchers.
// It will not save the parent field of the node. Or there will
// be cyclic dependencies issue for the json package.
func (s *store) Save() ([]byte, error) {
	s.worldLock.Lock()

	clonedStore := newStore()
	clonedStore.currentIndex = s.currentIndex
	clonedStore.root = s.root.Clone()
	clonedStore.watcherHub = s.watcherHub.clone()
	clonedStore.stats = s.stats.clone()
	clonedStore.currentVersion = s.currentVersion

	s.worldLock.Unlock()

	b, err := json.Marshal(clonedStore)

	if err != nil {
		return nil, err
	}

	return b, nil
}

// Recovery recovers the store system from a static state
// It needs to recover the parent field of the nodes.
// It needs to delete the expired nodes since the saved time and also
// needs to create monitoring go routines.
func (s *store) Recovery(state []byte) error {
	s.worldLock.Lock()
	defer s.worldLock.Unlock()
	err := json.Unmarshal(state, s)

	if err != nil {
		return err
	}

	s.ttlKeyHeap = newTtlKeyHeap()

	s.root.recoverAndclean()
	return nil
}

func (s *store) JsonStats() []byte {
	s.stats.Watchers = uint64(s.watcherHub.count)
	return s.stats.toJson()
}

func (s *store) TotalTransactions() uint64 {
	return s.stats.TotalTranscations()
}
