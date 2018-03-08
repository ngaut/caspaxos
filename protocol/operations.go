package protocol

import (
	"context"
	"math/rand"

	"github.com/go-kit/kit/log"
	"github.com/pkg/errors"
)

// Proposer models a concrete proposer.
type Proposer interface {
	// Propose is the primary API for clients. All changes including reads are
	// sent this way.
	Propose(ctx context.Context, key string, f ChangeFunc) (state []byte, b Ballot, err error)

	// These methods are for configuration changes.
	AddAccepter(target Acceptor) error
	AddPreparer(target Acceptor) error
	RemovePreparer(target Acceptor) error
	RemoveAccepter(target Acceptor) error

	// These methods are for garbage collection, for deletes.
	FullIdentityRead(ctx context.Context, key string) (state []byte, err error)
	InvalidateAndIncrement(ctx context.Context, key string) (Age, error)
}

// Acceptor models a complete, uniquely-addressable acceptor.
//
// Here we have a little fun with names: use Acceptor (-or) as a noun, to model
// the whole composite acceptor, and Accepter (-er) as a verb, to model the
// second-phase "accept" responsibilities only.
type Acceptor interface {
	Addresser
	Preparer
	Accepter
	GarbageCollector
}

// Addresser models something with a unique address.
type Addresser interface {
	Address() string // typically "protocol://host:port"
}

// Preparer models the first-phase responsibilities of an acceptor.
type Preparer interface {
	Prepare(ctx context.Context, key string, age Age, b Ballot) (value []byte, current Ballot, err error)
}

// Accepter models the second-phase responsibilities of an acceptor.
type Accepter interface {
	Accept(ctx context.Context, key string, age Age, b Ballot, value []byte) error
}

// GarbageCollector models the garbage collection responsibilities of an acceptor.
type GarbageCollector interface {
	RejectByAge(ctx context.Context, ages []Age) error
	RemoveIfEqual(ctx context.Context, key string, t Tombstone) error
}

// Assign special meaning to the zero/empty key "", which we use to increment
// ballot numbers for operations like changing cluster configuration.
const zerokey = ""

// Note: When growing (or shrinking) a cluster from an odd number of acceptors
// to an even number of acceptors, the implemented process is required. But when
// growing (or shrinking) a cluster from an even number of acceptors to an odd
// number of acceptors, an optimization is possible: we can first change the
// accept and prepare lists of all proposers, and then turn the acceptor on, and
// avoid the cost of a read.
//
// This is what's meant in this section of the paper: "The protocol for changing
// the set of acceptors from A_1...A_2F+2 to A_1...A_2F+3 [from even to odd] is
// more straightforward because we can treat a 2F+2 nodes cluster as a 2F+3
// nodes cluster where one node had been down from the beginning: [that process
// is] (1) Connect to each proposer and update its configuration to send the
// prepare and accept messages to the [second] A_1...A_2F+3 set of acceptors;
// (2) Turn on the A_2F+3 acceptor."
//
// I've chosen not to implement this for several reasons. First, cluster
// membership changes are rare and operator-driven, and so don't really benefit
// from the lower latency as much as reads or writes would. Second, the number
// of acceptors in the cluster is not known a priori, and can in theory drift
// between different proposers; calculating the correct value is difficult in
// itself, probably requiring asking some other source of authority. Third, in
// production environments, there's great value in having a consistent process
// for any cluster change; turning a node on at different points in that process
// depending on the cardinality of the node-set is fraught with peril.

// GrowCluster adds the target acceptor to the cluster of proposers.
func GrowCluster(ctx context.Context, target Acceptor, proposers []Proposer) error {
	// If we fail, try to leave the cluster in its original state.
	var undo []func()
	defer func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()

	// From the paper: "Connect to each proposer and update its configuration to
	// send the 'accept' messages to the [new] set of acceptors, and to require
	// F+2 confirmations during the 'accept' phase."
	for _, proposer := range proposers {
		if err := proposer.AddAccepter(target); err != nil {
			return errors.Wrap(err, "during grow step 1 (add accepter)")
		}
		undo = append(undo, func() { proposer.RemoveAccepter(target) })
	}

	// From the paper: "Pick any proposer and execute the identity state
	// transaction x -> x."
	var (
		proposer = proposers[rand.Intn(len(proposers))]
		identity = func(x []byte) []byte { return x }
	)
	if _, _, err := proposer.Propose(ctx, zerokey, identity); err != nil {
		return errors.Wrap(err, "during grow step 2 (identity read)")
	}

	// From the paper: "Connect to each proposer and update its configuration to
	// send 'prepare' messages to the [new] set of acceptors, and to require F+2
	// confirmations [during the 'prepare' phase]."
	for _, proposer := range proposers {
		if err := proposer.AddPreparer(target); err != nil {
			return errors.Wrap(err, "during grow step 3 (add preparer)")
		}
		undo = append(undo, func() { proposer.RemovePreparer(target) })
	}

	// Success! Kill the undo stack, and return.
	undo = []func(){}
	return nil
}

// ShrinkCluster removes the target acceptor from the cluster of proposers.
func ShrinkCluster(ctx context.Context, target Acceptor, proposers []Proposer) error {
	// If we fail, try to leave the cluster in its original state.
	var undo []func()
	defer func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()

	// From the paper: "The same steps [for growing the cluster] should be
	// executed in the reverse order to reduce the size of the cluster."

	// So, remove it as a preparer.
	for _, proposer := range proposers {
		if err := proposer.RemovePreparer(target); err != nil {
			return errors.Wrap(err, "during shrink step 1 (remove preparer)")
		}
		undo = append(undo, func() { proposer.AddPreparer(target) })
	}

	// Execute a no-op read.
	var (
		identity = func(x []byte) []byte { return x }
		proposer = proposers[rand.Intn(len(proposers))]
	)
	if _, _, err := proposer.Propose(ctx, zerokey, identity); err != nil {
		return errors.Wrap(err, "during shrink step 2 (identity read)")
	}

	// And then remove it as an accepter.
	for _, proposer := range proposers {
		if err := proposer.RemoveAccepter(target); err != nil {
			return errors.Wrap(err, "during shrink step 3 (remove accepter)")
		}
		undo = append(undo, func() { proposer.AddAccepter(target) })
	}

	// Done.
	undo = []func(){}
	return nil
}

// Tombstone represents the terminal form of a key. Propose some state, likely
// empty, and collect it with the resulting ballot into a Tombstone, which
// becomes input to the garbage collection process.
type Tombstone struct {
	Ballot Ballot
	State  []byte
}

// GarbageCollect removes an empty key as described in section 3.1 "How to
// delete a record" in the paper. The caller is responsible for recovering from
// errors.
func GarbageCollect(ctx context.Context, key string, t Tombstone, proposers []Proposer, acceptors []Acceptor, logger log.Logger) error {
	// From the paper: "(a) Replicates an empty value to all nodes by
	// executing the identity transform with max quorum size (2F+1)."
	{
		var (
			proposer = proposers[rand.Intn(len(proposers))]
			_, err   = proposer.FullIdentityRead(ctx, key)
		)
		if err != nil {
			return errors.Wrap(err, "error executing identity transform")
		}
	}

	// From the paper: "(b) Connects to each proposer, invalidates its cache
	// associated with the removing key, ... fast-forwards its counter to
	// guarantee that new ballot numbers are greater than the tombstone's
	// ballot, and increments proposer's age."
	var ages []Age
	{
		type result struct {
			age Age
			err error
		}
		results := make(chan result, len(proposers))
		for _, proposer := range proposers {
			go func(proposer Proposer) {
				age, err := proposer.InvalidateAndIncrement(ctx, key)
				results <- result{age, err}
			}(proposer)
		}
		for i := 0; i < cap(results); i++ {
			result := <-results
			if result.err != nil {
				return errors.Wrap(result.err, "error invalidating and incrementing proposer")
			}
			ages = append(ages, result.age)
		}
	}

	// From the paper: "(c) For each acceptor, asks to reject messages from
	// proposers if their age is younger than the corresponding age from the
	// previous step."
	{
		results := make(chan error, len(acceptors))
		for _, acceptor := range acceptors {
			go func(acceptor Acceptor) {
				results <- acceptor.RejectByAge(ctx, ages)
			}(acceptor)
		}
		for i := 0; i < cap(results); i++ {
			if err := <-results; err != nil {
				return errors.Wrap(err, "error updating ages in acceptor")
			}
		}
	}

	// From the paper: "(d) For each acceptor, remove the register if its
	// value is the tombstone from the 2a step."
	{
		results := make(chan error, len(acceptors))
		for _, acceptor := range acceptors {
			go func(acceptor Acceptor) {
				results <- acceptor.RemoveIfEqual(ctx, key, t)
			}(acceptor)
		}
		for i := 0; i < cap(results); i++ {
			if err := <-results; err != nil {
				return errors.Wrap(err, "error removing value from acceptor")
			}
		}
	}

	// Done!
	return nil
}
