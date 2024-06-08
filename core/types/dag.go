package types

import (
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/holiman/uint256"
	"golang.org/x/exp/slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// TxDep store the current tx dependency relation with other txs
type TxDep struct {
	// It describes the Relation with below txs
	// 0: this tx depends on below txs
	// 1: this transaction does not depend on below txs, all other previous txs depend on
	Relation  uint8
	TxIndexes []int
}

func (d *TxDep) AppendDep(i int) {
	d.TxIndexes = append(d.TxIndexes, i)
}

func (d *TxDep) Exist(i int) bool {
	for _, index := range d.TxIndexes {
		if index == i {
			return true
		}
	}

	return false
}

// TxDAG indicate how to use the dependency of txs
type TxDAG struct {
	// The TxDAG type
	// 0: delay the distribution of GasFee, it will ignore all gas fee distribution when tx execute
	// 1: timely distribution of transaction fees, it will keep partial serial execution when tx cannot delay the distribution
	Type uint8
	// Tx Dependency List, the list index is equal to TxIndex
	TxDeps []TxDep
}

func NewTxDAG(txLen int) *TxDAG {
	return &TxDAG{
		Type:   0,
		TxDeps: make([]TxDep, txLen),
	}
}

func (d *TxDAG) String() string {
	builder := strings.Builder{}
	exePaths := d.travelExecutionPaths()
	for _, path := range exePaths {
		builder.WriteString(fmt.Sprintf("%v\n", path))
	}
	return builder.String()
}

func (d *TxDAG) travelExecutionPaths() [][]int {
	// regenerate TxDAG
	nd := NewTxDAG(len(d.TxDeps))
	for i, txDep := range d.TxDeps {
		nd.TxDeps[i].Relation = 0
		if txDep.Relation == 0 {
			nd.TxDeps[i] = txDep
			continue
		}

		// recover to relation 0
		for j := 0; j < i; j++ {
			if !txDep.Exist(j) {
				nd.TxDeps[i].AppendDep(j)
			}
		}
	}
	exePaths := make([][]int, 0)

	// travel tx deps with BFS
	for i := 0; i < len(nd.TxDeps); i++ {
		exePaths = append(exePaths, travelTargetPath(nd.TxDeps, i))
	}
	return exePaths
}

func EvaluateTxDAG(dag *TxDAG, stats []*ExeStat) string {
	if len(stats) != len(dag.TxDeps) {
		return ""
	}
	sb := strings.Builder{}
	paths := dag.travelExecutionPaths()
	// Attention: with the worst schedule, it means the path is executed in sequential
	// if using best schedule, it will reduce a lot by executing previous txs in parallel
	var (
		maxTime int64
		maxGas  uint64
		maxRead int
		maxPath []int
	)
	for i, path := range paths {
		if stats[i].mustSerialFlag {
			continue
		}
		t, g, r := int64(0), uint64(0), 0
		for _, index := range path {
			t += stats[index].costTime
			g += stats[index].usedGas
			r += stats[index].readCount
		}
		sb.WriteString(fmt.Sprintf("Tx%v, %.3fms|%vgas|%vreads\npath: %v\n", i, float64(t)/1000, g, r, path))
		if t > maxTime {
			maxTime = t
			maxGas = g
			maxRead = r
			maxPath = path
		}
	}

	sb.WriteString(fmt.Sprintf("LongestParallelPath, %.3fms|%vgas|%vreads\npath: %v\n", float64(maxTime)/1000, maxGas, maxRead, maxPath))
	// serial path
	var (
		sTime int64
		sGas  uint64
		sRead int
		sPath []int
	)
	for i, stat := range stats {
		if stat.mustSerialFlag {
			continue
		}
		sPath = append(sPath, i)
		sTime += stat.costTime
		sGas += stat.usedGas
		sRead += stat.readCount
	}
	if sTime == 0 {
		return ""
	}
	sb.WriteString(fmt.Sprintf("LongestSerialPath, %.3fms|%vgas|%vreads\npath: %v\n", float64(sTime)/1000, sGas, sRead, sPath))
	sb.WriteString(fmt.Sprintf("Estimated saving: %.3fms, %.2f%%\n", float64(sTime-maxTime)/1000, float64(sTime-maxTime)/float64(sTime)*100))
	return sb.String()
}

func travelTargetPath(deps []TxDep, from int) []int {
	q := make([]int, 0, len(deps))
	path := make([]int, 0, len(deps))

	q = append(q, from)
	path = append(path, from)
	for len(q) > 0 {
		t := make([]int, 0, len(deps))
		for _, i := range q {
			for _, dep := range deps[i].TxIndexes {
				if !slices.Contains(path, dep) {
					path = append(path, dep)
					t = append(t, dep)
				}
			}
		}
		q = t
	}
	sort.Ints(path)
	return path
}

type ValidatorExtraItem struct {
	ValidatorAddress common.Address
	VoteAddress      BLSPublicKey
}

type HeaderCustomExtra struct {
	ValidatorSet ValidatorExtraItem
	TxDAG        TxDAG
}

// StateVersion record specific TxIndex & TxIncarnation
// if TxIndex equals to -1, it means the state read from DB.
type StateVersion struct {
	TxIndex       int
	TxIncarnation int
}

// ReadRecord keep read value & its version
type ReadRecord struct {
	StateVersion
	Val interface{}
}

// WriteRecord keep latest state value & change count
type WriteRecord struct {
	Val   interface{}
	Count int
}

// RWSet record all read & write set in txs
// Attention: this is not a concurrent safety structure
type RWSet struct {
	ver      StateVersion
	readSet  map[RWKey]*ReadRecord
	writeSet map[RWKey]*WriteRecord

	// some flags
	mustSerial bool
}

func NewRWSet(ver StateVersion) *RWSet {
	return &RWSet{
		ver:      ver,
		readSet:  make(map[RWKey]*ReadRecord),
		writeSet: make(map[RWKey]*WriteRecord),
	}
}

func (s *RWSet) RecordRead(key RWKey, ver StateVersion, val interface{}) {
	// only record the first read version
	if _, exist := s.readSet[key]; exist {
		return
	}
	s.readSet[key] = &ReadRecord{
		StateVersion: ver,
		Val:          val,
	}
}

func (s *RWSet) RecordWrite(key RWKey, val interface{}) {
	wr, exist := s.writeSet[key]
	if !exist {
		s.writeSet[key] = &WriteRecord{
			Val:   val,
			Count: 1,
		}
		return
	}
	wr.Val = val
	wr.Count++
}

func (s *RWSet) RevertWrite(key RWKey, val []byte) {
	wr, exist := s.writeSet[key]
	if !exist {
		return
	}
	if wr.Count == 1 {
		delete(s.writeSet, key)
		return
	}
	wr.Val = val
	wr.Count--
}

func (s *RWSet) Version() StateVersion {
	return s.ver
}

func (s *RWSet) ReadSet() map[RWKey]*ReadRecord {
	return s.readSet
}

func (s *RWSet) WriteSet() map[RWKey]*WriteRecord {
	return s.writeSet
}

func (s *RWSet) WithSerialFlag() *RWSet {
	s.mustSerial = true
	return s
}

func (s *RWSet) String() string {
	builder := strings.Builder{}
	builder.WriteString(fmt.Sprintf("tx: %v, inc: %v\nreadSet: [", s.ver.TxIndex, s.ver.TxIncarnation))
	i := 0
	for key, _ := range s.readSet {
		if i > 0 {
			builder.WriteString(fmt.Sprintf(", %v", key.String()))
			continue
		}
		builder.WriteString(fmt.Sprintf("%v", key.String()))
		i++
	}
	builder.WriteString("]\nwriteSet: [")
	i = 0
	for key, _ := range s.writeSet {
		if i > 0 {
			builder.WriteString(fmt.Sprintf(", %v", key.String()))
			continue
		}
		builder.WriteString(fmt.Sprintf("%v", key.String()))
		i++
	}
	builder.WriteString("]\n")
	return builder.String()
}

const (
	AccountStatePrefix = 'a'
	StorageStatePrefix = 's'
)

type RWKey [1 + common.AddressLength + common.HashLength]byte

type AccountState byte

const (
	AccountNonce AccountState = iota
	AccountBalance
	AccountCodeHash
	AccountSuicide
)

func AccountStateKey(account common.Address, state AccountState) RWKey {
	var key RWKey
	key[0] = AccountStatePrefix
	copy(key[1:], account.Bytes())
	key[1+common.AddressLength] = byte(state)
	return key
}

func StorageStateKey(account common.Address, state common.Hash) RWKey {
	var key RWKey
	key[0] = StorageStatePrefix
	copy(key[1:], account.Bytes())
	copy(key[1+common.AddressLength:], state.Bytes())
	return key
}

func (key *RWKey) IsAccountState() (bool, AccountState) {
	return AccountStatePrefix == key[0], AccountState(key[1+common.AddressLength])
}

func (key *RWKey) IsStorageState() bool {
	return StorageStatePrefix == key[0]
}

func (key *RWKey) String() string {
	return hex.EncodeToString(key[:])
}

type PendingWrite struct {
	Ver StateVersion
	Val interface{}
}

func NewPendingWrite(ver StateVersion, wr *WriteRecord) *PendingWrite {
	return &PendingWrite{
		Ver: ver,
		Val: wr.Val,
	}
}

func (w *PendingWrite) TxIndex() int {
	return w.Ver.TxIndex
}

func (w *PendingWrite) TxIncarnation() int {
	return w.Ver.TxIncarnation
}

type PendingWrites struct {
	list []*PendingWrite
}

func NewPendingWrites() *PendingWrites {
	return &PendingWrites{
		list: make([]*PendingWrite, 0),
	}
}

func (w *PendingWrites) Append(pw *PendingWrite) {
	if i, found := w.SearchTxIndex(pw.TxIndex()); found {
		w.list[i] = pw
		return
	}

	w.list = append(w.list, pw)
	for i := len(w.list) - 1; i > 0; i-- {
		if w.list[i].TxIndex() > w.list[i-1].TxIndex() {
			break
		}
		w.list[i-1], w.list[i] = w.list[i], w.list[i-1]
	}
}

func (w *PendingWrites) SearchTxIndex(txIndex int) (int, bool) {
	n := len(w.list)
	i, j := 0, n
	for i < j {
		h := int(uint(i+j) >> 1)
		// i ≤ h < j
		if w.list[h].TxIndex() < txIndex {
			i = h + 1
		} else {
			j = h
		}
	}
	return i, i < n && w.list[i].TxIndex() == txIndex
}

func (w *PendingWrites) FindLastWrite(txIndex int) *PendingWrite {
	var i, _ = w.SearchTxIndex(txIndex)
	for j := i - 1; j >= 0; j-- {
		if w.list[j].TxIndex() < txIndex {
			return w.list[j]
		}
	}

	return nil
}

type MVStates struct {
	rwSets          []*RWSet
	stats           []*ExeStat
	pendingWriteSet map[RWKey]*PendingWrites
	lock            sync.RWMutex
}

func NewMVStates(txCount int) *MVStates {
	return &MVStates{
		rwSets:          make([]*RWSet, txCount),
		stats:           make([]*ExeStat, txCount),
		pendingWriteSet: make(map[RWKey]*PendingWrites, txCount*8),
	}
}

func (s *MVStates) RWSets() []*RWSet {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.rwSets
}

func (s *MVStates) Stats() []*ExeStat {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.stats
}

func (s *MVStates) RWSet(index int) *RWSet {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.rwSets[index]
}

func (s *MVStates) FulfillRWSet(rwSet *RWSet, stat *ExeStat) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	index := rwSet.ver.TxIndex
	if index >= len(s.rwSets) {
		return errors.New("refill out of bound")
	}
	if s := s.rwSets[index]; s != nil {
		return errors.New("refill a exist RWSet")
	}
	if stat != nil {
		if stat.txIndex != index {
			return errors.New("wrong execution stat")
		}
		s.stats[index] = stat
	}

	for k, v := range rwSet.writeSet {
		// ignore no changed write record
		if rwSet.readSet[k] == nil || v == nil {
			log.Info("FulfillRWSet find read nil", "k", k.String())
		}
		if rwSet.readSet[k] != nil && isEqualRWVal(k, rwSet.readSet[k].Val, v.Val) {
			delete(rwSet.writeSet, k)
			continue
		}
		if _, exist := s.pendingWriteSet[k]; !exist {
			s.pendingWriteSet[k] = NewPendingWrites()
		}
		s.pendingWriteSet[k].Append(NewPendingWrite(rwSet.ver, v))
	}
	s.rwSets[index] = rwSet
	return nil
}

func isEqualRWVal(key RWKey, src interface{}, compared interface{}) bool {
	if ok, state := key.IsAccountState(); ok {
		switch state {
		case AccountBalance:
			//if !isNil(src) && !isNil(compared) {
			//	return src.(*uint256.Int).Eq(compared.(*uint256.Int))
			//}
			//return src == compared
			if src != nil && compared != nil {
				return equalUint256(src.(*uint256.Int), compared.(*uint256.Int))
			}
			return src == compared
		case AccountNonce:
			return src.(uint64) == compared.(uint64)
		case AccountCodeHash:
			if src != nil && compared != nil {
				return slices.Equal(src.([]byte), compared.([]byte))
			}
			return src == compared
		}
		return false
	}

	if src != nil && compared != nil {
		return src.(common.Hash) == compared.(common.Hash)
	}
	return src == compared
}

func equalUint256(s, c *uint256.Int) bool {
	if s != nil && c != nil {
		return s.Eq(c)
	}

	return s == c
}

func (s *MVStates) ResolveDAG() *TxDAG {
	rwSets := s.RWSets()
	txDAG := NewTxDAG(len(rwSets))
	for i := len(rwSets) - 1; i >= 0; i-- {
		txDAG.TxDeps[i].TxIndexes = []int{}
		if rwSets[i].mustSerial {
			txDAG.TxDeps[i].Relation = 1
			continue
		}
		readSet := rwSets[i].ReadSet()
		// check if there has written op before i
		// TODO: check suicide
		// add read address flag, it only for check suicide quickly, and cannot for other scenarios.
		for j := 0; j < i; j++ {
			// check tx dependency, only check key, skip version
			for k, _ := range rwSets[j].WriteSet() {
				if _, ok := readSet[k]; ok {
					txDAG.TxDeps[i].AppendDep(j)
					break
				}
			}
		}
	}

	return txDAG
}

type ExeStat struct {
	txIndex        int
	usedGas        uint64
	readCount      int
	startTime      int64
	costTime       int64
	mustSerialFlag bool
}

func NewExeStat(txIndex int) *ExeStat {
	return &ExeStat{
		txIndex: txIndex,
	}
}

func (s *ExeStat) Begin() *ExeStat {
	s.startTime = time.Now().UnixMicro()
	return s
}

func (s *ExeStat) Done() *ExeStat {
	s.costTime = time.Now().UnixMicro() - s.startTime
	return s
}

func (s *ExeStat) WithSerialFlag() *ExeStat {
	s.mustSerialFlag = true
	return s
}

func (s *ExeStat) WithGas(gas uint64) *ExeStat {
	s.usedGas = gas
	return s
}

func (s *ExeStat) WithRead(rc int) *ExeStat {
	s.readCount = rc
	return s
}
