package kvraft

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"../labgob"
	"../labrpc"
	"../raft"
)

const (
	Debug = 0

	numChecks     = 3
	checkInterval = 150 * time.Millisecond
)

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		log.Printf(format, a...)
	}
	return
}

// Op would be passed as argument to rf.Start(). Thus, when receiving ApplyMsg,
// the Op can be retrived by applyMsg.Command.
type Op struct {
	// Your definitions here.
	OpType string

	Key   string
	Value string
	OpID  int64
}

type Record struct {
	Err Err

	// Only used by Get
	Value string
}

type KVServer struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg
	dead    int32 // set by Kill()

	maxraftstate int // snapshot if log grows this big

	// Your definitions here.

	// key-value database
	db map[string]string

	// execution history to handle duplicate requests
	execRecord map[int64]Record
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	kv.mu.Lock()
	defer kv.mu.Unlock()

	term1, isLeader := kv.rf.GetState()
	if !isLeader {
		reply.Err = ErrWrongLeader
		return
	}

	// Check for duplicate request
	if record, duplicate := kv.execRecord[args.ID]; duplicate {
		reply.Err = record.Err
		reply.Value = record.Value
		return
	}

	// Start the consensus process on Raft
	opID := args.ID
	op := Op{
		Key:    args.Key,
		OpID:   opID,
		OpType: "Get",
	}
	kv.rf.Start(op)

	// Wait for the opreation to be committed and applied
	for i := 0; i < numChecks; i++ {
		kv.mu.Unlock()
		time.Sleep(checkInterval)
		kv.mu.Lock()

		// Evicted, retry
		term2, _ := kv.rf.GetState()
		if term2 != term1 {
			reply.Err = ErrWrongLeader
			return
		}

		record, ok := kv.execRecord[opID]
		if ok {
			reply.Err = record.Err
			reply.Value = record.Value
			return
		}
	}

	// Not commited/applied, make client retry
	reply.Err = ErrWrongLeader
}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	kv.mu.Lock()
	defer kv.mu.Unlock()

	term1, isLeader := kv.rf.GetState()
	if !isLeader {
		reply.Err = ErrWrongLeader
		return
	}

	// Check for duplicate request
	if record, duplicate := kv.execRecord[args.ID]; duplicate {
		reply.Err = record.Err
		return
	}

	opID := args.ID
	op := Op{
		Key:    args.Key,
		Value:  args.Value,
		OpID:   opID,
		OpType: args.Op,
	}
	kv.rf.Start(op)

	// Wait for the opreation to be committed and applied
	for i := 0; i < numChecks; i++ {
		kv.mu.Unlock()
		time.Sleep(checkInterval)
		kv.mu.Lock()

		// Evicted, retry
		term2, _ := kv.rf.GetState()
		if term2 != term1 {
			reply.Err = ErrWrongLeader
			return
		}

		record, ok := kv.execRecord[opID]
		if ok {
			reply.Err = record.Err
			return
		}
	}

	// Not commited/applied, make client retry
	reply.Err = ErrWrongLeader
}

//
// the tester calls Kill() when a KVServer instance won't
// be needed again. for your convenience, we supply
// code to set rf.dead (without needing a lock),
// and a killed() method to test rf.dead in
// long-running loops. you can also add your own
// code to Kill(). you're not required to do anything
// about this, but it may be convenient (for example)
// to suppress debug output from a Kill()ed instance.
//
func (kv *KVServer) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *KVServer) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

// This goroutine reads ApplyMsg from kv.applyCh and store them in kv.msgs.
func (kv *KVServer) readApplyChRoutine() {
	for {
		msg := <-kv.applyCh
		op := msg.Command.(Op)

		kv.mu.Lock()

		// Apply Op, if not duplicate
		if _, duplicate := kv.execRecord[op.OpID]; !duplicate {
			switch op.OpType {
			case "Get":
				value, ok := kv.db[op.Key]
				if !ok {
					kv.execRecord[op.OpID] = Record{ErrNoKey, ""}
				} else {
					kv.execRecord[op.OpID] = Record{OK, value}
				}
			case "Put":
				kv.db[op.Key] = op.Value
				kv.execRecord[op.OpID] = Record{OK, ""}
			case "Append":
				if _, ok := kv.db[op.Key]; !ok {
					kv.db[op.Key] = ""
				}
				kv.db[op.Key] += op.Value
				kv.execRecord[op.OpID] = Record{OK, ""}
			default:
				DPrintf("Panic: Unexpected")
			}
		}

		kv.mu.Unlock()

		DPrintf("[%v] recieves %+v from Raft instance", kv.me, op)
	}
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Raft to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
// the k/v server should snapshot when Raft's saved state exceeds maxraftstate bytes,
// in order to allow Raft to garbage-collect its log. if maxraftstate is -1,
// you don't need to snapshot.
// StartKVServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *KVServer {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate

	// You may need initialization code here.

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	// You may need initialization code here.
	kv.db = make(map[string]string)
	kv.execRecord = make(map[int64]Record)

	go kv.readApplyChRoutine()

	return kv
}
