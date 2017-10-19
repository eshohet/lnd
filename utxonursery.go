package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

//                          SUMMARY OF OUTPUT STATES
//
//  - CRIB (babyOutput) outputs are two-stage htlc outputs that are initially
//    locked using a CLTV delay, followed by a CSV delay. The first stage of a
//    crib output requires broadcasting a presigned htlc timeout txn generated
//    by the wallet after an absolute expiry height. Since the timeout txns are
//    predetermined, they cannot be batched after-the-fact, meaning that all
//    CRIB outputs are broadcast and confirmed independently. After the first
//    stage is complete, a CRIB output is moved to the KNDR state, which will
//    finishing sweeping the second-layer CSV delay.
//
//  - PSCL (kidOutput) outputs are commitment outputs locked under a CSV delay.
//    These outputs are stored temporarily in this state until the commitment
//    transaction confirms, as this solidifies an absolute height that the
//    relative time lock will expire. Once this maturity height is determined,
//    the PSCL output is moved into KNDR.
//
//  - KNDR (kidOutput) outputs are CSV delayed outputs for which the maturity
//    height has been fully determined. This results from having received
//    confirmation of the UTXO we are trying to spend, contained in either the
//    commitment txn or htlc timeout txn. Once the maturity height is reached,
//    the utxo nursery will sweep all KNDR outputs scheduled for that height
//    using a single txn.
//
//    NOTE: Due to the fact that KNDR outputs can be dynamically aggregated and
//    swept, we make precautions to finalize the KNDR outputs at a particular
//    height on our first attempt to sweep it. Finalizing involves signing the
//    sweep transaction and persisting it in the nursery store, and recording
//    the last finalized height. Any attempts to replay an already finalized
//    height will result in broadcasting the already finalized txn, ensuring the
//    nursery does not broadcast different txids for the same batch of KNDR
//    outputs. The reason txids may change is due to the probabilistic nature of
//    generating the pkscript in the sweep txn's output, even if the set of
//    inputs remains static across attempts.
//
//  - GRAD (kidOutput) outputs are KNDR outputs that have successfully been
//    swept into the user's wallet. A channel is considered mature once all of
//    its outputs, including two-stage htlcs, have entered the GRAD state,
//    indicating that it safe to mark the channel as fully closed.
//
//
//                     OUTPUT STATE TRANSITIONS IN UTXO NURSERY
//
//      ┌────────────────┐            ┌──────────────┐
//      │ Commit Outputs │            │ HTLC Outputs │
//      └────────────────┘            └──────────────┘
//               │                            │
//               │                            │
//               │                            │               UTXO NURSERY
//   ┌───────────┼────────────────┬───────────┼───────────────────────────────┐
//   │           │                            │                               │
//   │           │                │           │                               │
//   │           │                            │           CLTV-Delayed        │
//   │           │                │           V            babyOutputs        │
//   │           │                        ┌──────┐                            │
//   │           │                │       │ CRIB │                            │
//   │           │                        └──────┘                            │
//   │           │                │           │                               │
//   │           │                            │                               │
//   │           │                │           |                               │
//   │           │                            V    Wait CLTV                  │
//   │           │                │         ▕[ ]       +                      │
//   │           │                            |   Publish Txn                 │
//   │           │                │           │                               │
//   │           │                            │                               │
//   │           │                │           V ┌ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┐         │
//   │           │                           ( )  waitForEnrollment           │
//   │           │                │           | └ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘         │
//   │           │                            │                               │
//   │           │                │           │                               │
//   │           │                            │                               │
//   │           V                │           │                               │
//   │       ┌──────┐                         │                               │
//   │       │ PSCL │             └  ──  ──  ─┼  ──  ──  ──  ──  ──  ──  ──  ─┤
//   │       └──────┘                         │                               │
//   │           │                            │                               │
//   │           │                            │                               │
//   │           V ┌ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┐    │            CSV-Delayed        │
//   │          ( )   waitForPromotion        │             kidOutputs        │
//   │           | └ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘    │                               │
//   │           │                            │                               │
//   │           │                            │                               │
//   │           │                            V                               │
//   │           │                        ┌──────┐                            │
//   │           └─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─▶│ KNDR │                            │
//   │                                    └──────┘                            │
//   │                                        │                               │
//   │                                        │                               │
//   │                                        |                               │
//   │                                        V     Wait CSV                  │
//   │                                      ▕[ ]       +                      │
//   │                                        |   Publish Txn                 │
//   │                                        │                               │
//   │                                        │                               │
//   │                                        V ┌ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┐         │
//   │                                       ( )  waitForGraduation           │
//   │                                        | └ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘         │
//   │                                        │                               │
//   │                                        │                               │
//   │                                        V                               │
//   │                                     ┌──────┐                           │
//   │                                     │ GRAD │                           │
//   │                                     └──────┘                           │
//   │                                        │                               │
//   │                                        │                               │
//   │                                        │                               │
//   └────────────────────────────────────────┼───────────────────────────────┘
//                                            │
//                                            │
//                                            │
//                                            │
//                                            V
//                                   ┌────────────────┐
//                                   │ Wallet Outputs │
//                                   └────────────────┘

var byteOrder = binary.BigEndian

var (
	// ErrContractNotFound is returned when the nursery is unable to
	// retreive information about a queried contract.
	ErrContractNotFound = fmt.Errorf("unable to locate contract")
)

// NurseryConfig abstracts the required subsystems used by the utxo nursery. An
// instance of NurseryConfig is passed to newUtxoNursery during instantiationn.
type NurseryConfig struct {
	// ChainIO is used by the utxo nursery to determine the current block
	// height, which drives the incubation of the nursery's outputs.
	ChainIO lnwallet.BlockChainIO

	// ConfDepth is the number of blocks the nursery store waits before
	// determining outputs in the chain as confirmed.
	ConfDepth uint32

	// PruningDepth is the number of blocks after which the nursery purges
	// its persistent state.
	PruningDepth uint32

	// DB provides access to a user's channels, such that they can be marked
	// fully closed after incubation has concluded.
	DB *channeldb.DB

	// Estimator is used when crafting sweep transactions to estimate the
	// necessary fee relative to the expected size of the sweep transaction.
	Estimator lnwallet.FeeEstimator

	// GenSweepScript generates a P2WKH script belonging to the wallet where
	// funds can be swept.
	GenSweepScript func() ([]byte, error)

	// Notifier provides the utxo nursery the ability to subscribe to
	// transaction confirmation events, which advance outputs through their
	// persistence state transitions.
	Notifier chainntnfs.ChainNotifier

	// PublishTransaction facilitates the process of broadcasting a signed
	// transaction to the appropriate network.
	PublishTransaction func(*wire.MsgTx) error

	// Signer is used by the utxo nursery to generate valid witnesses at the
	// time the incubated outputs need to be spent.
	Signer lnwallet.Signer

	// Store provides access to and modification of the persistent state
	// maintained about the utxo nursery's incubating outputs.
	Store NurseryStore
}

// utxoNursery is a system dedicated to incubating time-locked outputs created
// by the broadcast of a commitment transaction either by us, or the remote
// peer. The nursery accepts outputs and "incubates" them until they've reached
// maturity, then sweep the outputs into the source wallet. An output is
// considered mature after the relative time-lock within the pkScript has
// passed. As outputs reach their maturity age, they're swept in batches into
// the source wallet, returning the outputs so they can be used within future
// channels, or regular Bitcoin transactions.
type utxoNursery struct {
	started uint32
	stopped uint32

	cfg *NurseryConfig

	mu            sync.Mutex
	currentHeight uint32

	quit chan struct{}
	wg   sync.WaitGroup
}

// newUtxoNursery creates a new instance of the utxoNursery from a
// ChainNotifier and LightningWallet instance.
func newUtxoNursery(cfg *NurseryConfig) *utxoNursery {
	return &utxoNursery{
		cfg:  cfg,
		quit: make(chan struct{}),
	}
}

// Start launches all goroutines the utxoNursery needs to properly carry out
// its duties.
func (u *utxoNursery) Start() error {
	if !atomic.CompareAndSwapUint32(&u.started, 0, 1) {
		return nil
	}

	utxnLog.Tracef("Starting UTXO nursery")

	// 1. Flush all fully-graduated channels from the pipeline.

	// Load any pending close channels, which represents the super set of
	// all channels that may still be incubating.
	pendingCloseChans, err := u.cfg.DB.FetchClosedChannels(true)
	if err != nil {
		return err
	}

	// Ensure that all mature channels have been marked as fully closed in
	// the channeldb.
	for _, pendingClose := range pendingCloseChans {
		if err := u.closeAndRemoveIfMature(&pendingClose.ChanPoint); err != nil {
			return err
		}
	}

	// TODO(conner): check if any fully closed channels can be removed from
	// utxn.

	// Query the nursery store for the lowest block height we could be
	// incubating, which is taken to be the last height for which the
	// database was pruned.
	lastPrunedHeight, err := u.cfg.Store.LastPurgedHeight()
	if err != nil {
		return err
	}

	// 2. Restart spend ntfns for any preschool outputs, which are waiting
	// for the force closed commitment txn to confirm.
	//
	// NOTE: The next two steps *may* spawn go routines, thus from this
	// point forward, we must close the nursery's quit channel if we detect
	// any failures during startup to ensure they terminate.
	if err := u.reloadPreschool(lastPrunedHeight); err != nil {
		close(u.quit)
		return err
	}

	// 3. Replay all crib and kindergarten outputs from last pruned to
	// current best height.
	if err := u.reloadClasses(lastPrunedHeight); err != nil {
		close(u.quit)
		return err
	}

	// 4. Now that we are finalized, start watching for new blocks.

	// Register with the notifier to receive notifications for each newly
	// connected block. We register during startup to ensure that no blocks
	// are missed while we are handling blocks that were missed during the
	// time the UTXO nursery was unavailable.
	newBlockChan, err := u.cfg.Notifier.RegisterBlockEpochNtfn()
	if err != nil {
		close(u.quit)
		return err
	}

	u.wg.Add(1)
	go u.incubator(newBlockChan)

	return nil
}

// Stop gracefully shuts down any lingering goroutines launched during normal
// operation of the utxoNursery.
func (u *utxoNursery) Stop() error {
	if !atomic.CompareAndSwapUint32(&u.stopped, 0, 1) {
		return nil
	}

	utxnLog.Infof("UTXO nursery shutting down")

	close(u.quit)
	u.wg.Wait()

	return nil
}

// reloadPreschool re-initializes the chain notifier with all of the outputs
// that had been saved to the "preschool" database bucket prior to shutdown.
func (u *utxoNursery) reloadPreschool(heightHint uint32) error {
	psclOutputs, err := u.cfg.Store.FetchPreschools()
	if err != nil {
		return err
	}

	for i, kid := range psclOutputs {
		txID := kid.OutPoint().Hash

		confChan, err := u.cfg.Notifier.RegisterConfirmationsNtfn(
			&txID, u.cfg.ConfDepth, heightHint)
		if err != nil {
			return err
		}

		utxnLog.Infof("Preschool outpoint %v re-registered for confirmation "+
			"notification.", kid.OutPoint())

		u.wg.Add(1)
		go u.waitForPromotion(&psclOutputs[i], confChan)
	}

	return nil
}

// reloadClasses replays the graduation of all kindergarten and crib outputs for
// heights that have not been finalized.  This allows the nursery to
// reinitialize all state to continue sweeping outputs, even in the event that
// we missed blocks while offline. reloadClasses is called during the startup of
// the UTXO Nursery.
func (u *utxoNursery) reloadClasses(lastPrunedHeight uint32) error {
	// Get the most recently mined block.
	_, bestHeight, err := u.cfg.ChainIO.GetBestBlock()
	if err != nil {
		return err
	}

	// If we haven't yet seen any registered force closes, or we're already
	// caught up with the current best chain, then we can exit early.
	if lastPrunedHeight == 0 || uint32(bestHeight) == lastPrunedHeight {
		return nil
	}

	utxnLog.Infof("Processing outputs from missed blocks. Starting with "+
		"blockHeight: %v, to current blockHeight: %v", lastPrunedHeight,
		bestHeight)

	// Loop through and check for graduating outputs at each of the missed
	// block heights.
	for curHeight := lastPrunedHeight + 1; curHeight <= uint32(bestHeight); curHeight++ {
		// Each attempt at graduation is protected by a lock, since
		// there may be background processes attempting to modify the
		// database concurrently.
		u.mu.Lock()
		err := u.graduateClass(curHeight)
		u.mu.Unlock()

		if err != nil {
			utxnLog.Errorf("Failed to graduate outputs at height=%v: %v",
				curHeight, err)
			return err
		}
	}

	utxnLog.Infof("UTXO Nursery is now fully synced")

	return nil
}

// graduateClass handles the steps involved in spending outputs whose CSV or
// CLTV delay expires at the nursery's current height. This method is called
// each time a new block arrives, or during startup to catch up on heights we
// may have missed while the nursery was offline.
func (u *utxoNursery) graduateClass(classHeight uint32) error {

	// Record this height as the nursery's current best height.
	u.currentHeight = classHeight

	// Fetch all information about the crib and kindergarten outputs at this
	// height. In addition to the outputs, we also retrieve the finalized
	// kindergarten sweep txn, which will be nil if we have not attempted
	// this height before, or if no kindergarten outputs exist at this
	// height.
	finalTx, kgtnOutputs, cribOutputs, err := u.cfg.Store.FetchClass(
		classHeight)
	if err != nil {
		return err
	}

	// Load the last finalized height, so we can determine if the
	// kindergarten sweep txn should be crafted.
	lastFinalizedHeight, err := u.cfg.Store.LastFinalizedHeight()
	if err != nil {
		return err
	}

	// If we haven't processed this height before, we finalize the
	// graduating kindergarten outputs, by signing a sweep transaction that
	// spends from them. This txn is persisted such that we never broadcast
	// a different txn for the same height. This allows us to recover from
	// failures, and watch for the correct txid.
	if classHeight > lastFinalizedHeight {
		// If this height has never been finalized, we have never
		// generated a sweep txn for this height. Generate one if there
		// are kindergarten outputs to be spent.
		if len(kgtnOutputs) > 0 {
			finalTx, err = u.createSweepTx(kgtnOutputs)
			if err != nil {
				utxnLog.Errorf("Failed to create sweep txn at "+
					"height %d", classHeight)
				return err
			}
		}

		// Persist the kindergarten sweep txn to the nursery store. It
		// is safe to store a nil finalTx, which happens if there are no
		// graduating kindergarten outputs.
		err = u.cfg.Store.FinalizeKinder(classHeight, finalTx)
		if err != nil {
			utxnLog.Errorf("Failed to finalize height %d",
				classHeight)

			return err
		}

		// Log if the finalized transaction is non-trivial.
		if finalTx != nil {
			utxnLog.Infof("Finalized kindergarten at height %d ",
				classHeight)
		}
	}

	// Now that the kindergarten sweep txn has either been finalized or
	// restored, broadcast the txn, and set up notifications that will
	// transition the swept kindergarten outputs into graduated outputs.
	if finalTx != nil {
		utxnLog.Infof("Sweeping %v time-locked outputs "+
			"with sweep tx (txid=%v): %v", len(kgtnOutputs),
			finalTx.TxHash(), newLogClosure(func() string {
				return spew.Sdump(finalTx)
			}))

		err := u.sweepGraduatingKinders(classHeight, finalTx,
			kgtnOutputs)
		if err != nil {
			utxnLog.Errorf("Failed to sweep %d kindergarten outputs "+
				"at height %d: %v", len(kgtnOutputs), classHeight,
				err)
			return err
		}
	}

	// Now, we broadcast all pre-signed htlc txns from the crib outputs at
	// this height. There is no need to finalize these txns, since the txid
	// is predetermined when signed in the wallet.
	for i := range cribOutputs {
		err := u.sweepCribOutput(classHeight, &cribOutputs[i])
		if err != nil {
			utxnLog.Errorf("Failed to sweep crib output %v",
				cribOutputs[i].OutPoint())
			return err
		}
	}

	// Can't purge height below the reorg safety depth.
	if u.cfg.PruningDepth >= classHeight {
		return nil
	}

	// Otherwise, purge all state below our threshold height.
	heightToPurge := classHeight - u.cfg.PruningDepth
	if err := u.cfg.Store.PurgeHeight(heightToPurge); err != nil {
		utxnLog.Errorf("Failed to purge height %d", heightToPurge)
		return err
	}

	return nil
}

// craftSweepTx accepts accepts a list of kindergarten outputs, and signs and
// generates a signed txn that spends from them. This method also makes an
// accurate fee estimate before generating the required witnesses.
func (u *utxoNursery) createSweepTx(kgtnOutputs []kidOutput) (*wire.MsgTx, error) {
	// Create a transaction which sweeps all the newly mature outputs into
	// a output controlled by the wallet.
	// TODO(roasbeef): car be more intelligent about buffering outputs to
	// be more efficient on-chain.

	// Gather the CSV delayed inputs to our sweep transaction, and construct
	// an estimate for the weight of the sweep transaction.
	inputs := make([]CsvSpendableOutput, 0, len(kgtnOutputs))

	var txWeight uint64
	txWeight += 4*lnwallet.BaseSweepTxSize + lnwallet.WitnessHeaderSize

	for i := range kgtnOutputs {
		input := &kgtnOutputs[i]

		var witnessWeight uint64
		switch input.WitnessType() {
		case lnwallet.CommitmentTimeLock:
			witnessWeight = lnwallet.ToLocalTimeoutWitnessSize

		case lnwallet.HtlcOfferedTimeout:
			witnessWeight = lnwallet.OfferedHtlcTimeoutWitnessSize

		default:
			utxnLog.Warnf("kindergarten output in nursery store "+
				"contains unexpected witness type: %v",
				input.WitnessType())
			continue
		}

		txWeight += 4 * lnwallet.InputSize
		txWeight += witnessWeight

		inputs = append(inputs, input)
	}

	return u.sweepCsvSpendableOutputsTxn(txWeight, inputs)
}

// sweepCsvSpendableOutputsTxn creates a final sweeping transaction with all
// witnesses in place for all inputs using the provided txn fee. The created
// transaction has a single output sending all the funds back to the source
// wallet, after accounting for the fee estimate.
func (u *utxoNursery) sweepCsvSpendableOutputsTxn(txWeight uint64,
	inputs []CsvSpendableOutput) (*wire.MsgTx, error) {

	// Generate the receiving script to which the funds will be swept.
	pkScript, err := u.cfg.GenSweepScript()
	if err != nil {
		return nil, err
	}

	// Sum up the total value contained in the inputs.
	var totalSum btcutil.Amount
	for _, o := range inputs {
		totalSum += o.Amount()
	}

	// Using the txn weight estimate, compute the required txn fee.
	feePerWeight := u.cfg.Estimator.EstimateFeePerWeight(1)
	txFee := btcutil.Amount(txWeight * feePerWeight)

	// Sweep as much possible, after subtracting txn fees.
	sweepAmt := int64(totalSum - txFee)

	// Create the sweep transaction that we will be building. We use version
	// 2 as it is required for CSV. The txn will sweep the amount after fees
	// to the pkscript generated above.
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: pkScript,
		Value:    sweepAmt,
	})

	// Add all of our inputs, including the respective CSV delays.
	for _, input := range inputs {
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *input.OutPoint(),
			// TODO(roasbeef): assumes pure block delays
			Sequence: input.BlocksToMaturity(),
		})
	}

	// Before signing the transaction, check to ensure that it meets some
	// basic validity requirements.
	btx := btcutil.NewTx(sweepTx)
	if err := blockchain.CheckTransactionSanity(btx); err != nil {
		return nil, err
	}

	hashCache := txscript.NewTxSigHashes(sweepTx)

	// With all the inputs in place, use each output's unique witness
	// function to generate the final witness required for spending.
	addWitness := func(idx int, tso CsvSpendableOutput) error {
		witness, err := tso.BuildWitness(u.cfg.Signer, sweepTx, hashCache, idx)
		if err != nil {
			return err
		}

		sweepTx.TxIn[idx].Witness = witness

		return nil
	}

	for i, input := range inputs {
		if err := addWitness(i, input); err != nil {
			return nil, err
		}
	}

	return sweepTx, nil
}

// sweepGraduatingKinders generates and broadcasts the transaction that
// transfers control of funds from a channel commitment transaction to the
// user's wallet.
func (u *utxoNursery) sweepGraduatingKinders(classHeight uint32,
	finalTx *wire.MsgTx, kgtnOutputs []kidOutput) error {

	// With the sweep transaction fully signed, broadcast the transaction
	// to the network. Additionally, we can stop tracking these outputs as
	// they've just been swept.
	// TODO(conner): handle concrete error types returned from publication
	if err := u.cfg.PublishTransaction(finalTx); err != nil &&
		!strings.Contains(err.Error(), "TX rejected:") {
		utxnLog.Errorf("unable to broadcast sweep tx: %v, %v",
			err, spew.Sdump(finalTx))
		return err
	}

	finalTxID := finalTx.TxHash()

	utxnLog.Infof("Registering sweep tx %v for confs at height %d",
		finalTxID, classHeight)

	confChan, err := u.cfg.Notifier.RegisterConfirmationsNtfn(
		&finalTxID, u.cfg.ConfDepth, classHeight)
	if err != nil {
		utxnLog.Errorf("unable to register notification for "+
			"sweep confirmation: %v", finalTxID)
		return err
	}

	u.wg.Add(1)
	go u.waitForGraduation(classHeight, kgtnOutputs, confChan)

	return nil
}

// sweepCribOutput broadcasts the crib output's htlc timeout txn, and sets up a
// notification that will advance it to the kindergarten bucket upon
// confirmation.
func (u *utxoNursery) sweepCribOutput(classHeight uint32, baby *babyOutput) error {
	// Broadcast HTLC transaction
	// TODO(conner): handle concrete error types returned from publication
	err := u.cfg.PublishTransaction(baby.timeoutTx)
	if err != nil &&
		!strings.Contains(err.Error(), "TX rejected:") {
		utxnLog.Errorf("Unable to broadcast baby tx: "+
			"%v, %v", err,
			spew.Sdump(baby.timeoutTx))
		return err
	}

	birthTxID := baby.OutPoint().Hash

	// Register for the confirmation of presigned htlc txn.
	confChan, err := u.cfg.Notifier.RegisterConfirmationsNtfn(
		&birthTxID, u.cfg.ConfDepth, classHeight)
	if err != nil {
		return err
	}

	utxnLog.Infof("Baby output %v registered for promotion "+
		"notification.", baby.OutPoint())

	u.wg.Add(1)
	go u.waitForEnrollment(baby, confChan)

	return nil
}

// IncubateOutputs sends a request to utxoNursery to incubate the outputs
// defined within the summary of a closed channel. Individually, as all outputs
// reach maturity, they'll be swept back into the wallet.
func (u *utxoNursery) IncubateOutputs(closeSummary *lnwallet.ForceCloseSummary) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	var (
		commOutput  *kidOutput
		htlcOutputs = make([]babyOutput, 0, len(closeSummary.HtlcResolutions))
	)

	// 1. Build all the spendable outputs that we will try to incubate.

	// It could be that our to-self output was below the dust limit. In that
	// case the SignDescriptor would be nil and we would not have that
	// output to incubate.
	if closeSummary.SelfOutputSignDesc != nil {
		selfOutput := makeKidOutput(
			&closeSummary.SelfOutpoint,
			&closeSummary.ChanPoint,
			closeSummary.SelfOutputMaturity,
			lnwallet.CommitmentTimeLock,
			closeSummary.SelfOutputSignDesc,
		)

		// We'll skip any zero value'd outputs as this indicates we
		// don't have a settled balance within the commitment
		// transaction.
		if selfOutput.Amount() > 0 {
			commOutput = &selfOutput
		}
	}

	for i := range closeSummary.HtlcResolutions {
		htlcRes := closeSummary.HtlcResolutions[i]

		htlcOutpoint := &wire.OutPoint{
			Hash:  htlcRes.SignedTimeoutTx.TxHash(),
			Index: 0,
		}

		utxnLog.Infof("htlc resolution with expiry: %v",
			htlcRes.Expiry)

		htlcOutput := makeBabyOutput(
			htlcOutpoint,
			&closeSummary.ChanPoint,
			closeSummary.SelfOutputMaturity,
			lnwallet.HtlcOfferedTimeout,
			&htlcRes,
		)

		if htlcOutput.Amount() > 0 {
			htlcOutputs = append(htlcOutputs, htlcOutput)
		}

	}

	// If there are no outputs to incubate for this channel, we simply mark
	// the channel as fully closed.
	if commOutput == nil && len(htlcOutputs) == 0 {
		return u.cfg.DB.MarkChanFullyClosed(&closeSummary.ChanPoint)
	}

	// 2. Persist the outputs we intended to sweep in the nursery store
	if err := u.cfg.Store.Incubate(commOutput, htlcOutputs); err != nil {
		utxnLog.Infof("Unable to persist incubation of channel %v: %v",
			&closeSummary.ChanPoint, err)
		return err
	}

	// 3. If we are incubating a preschool output, register for a spend
	// notification that will transition it to the kindergarten bucket.
	if commOutput != nil {
		commitTxID := commOutput.OutPoint().Hash

		// Register for a notification that will trigger graduation from
		// preschool to kindergarten when the channel close transaction
		// has been confirmed.
		confChan, err := u.cfg.Notifier.RegisterConfirmationsNtfn(
			&commitTxID, u.cfg.ConfDepth, u.currentHeight)
		if err != nil {
			utxnLog.Errorf("Unable to register preschool output %v for "+
				"confirmation: %v", commitTxID, err)
			return err
		}

		utxnLog.Infof("Added kid output to pscl: %v",
			commOutput.OutPoint())

		// Launch a dedicated goroutine that will move the output from
		// the preschool bucket to the kindergarten bucket once the
		// channel close transaction has been confirmed.
		u.wg.Add(1)
		go u.waitForPromotion(commOutput, confChan)
	}

	return nil
}

// incubator is tasked with driving all state transitions that are dependent on
// the current height of the blockchain. As new blocks arrive, the incubator
// will attempt spend outputs at the latest height. The asynchronous
// confirmation of these spends will either 1) move a crib output into the
// kindergarten bucket or 2) move a kindergarten output into the graduated
// bucket. The incubator is also designed to purge all state below the config's
// PruningDepth to avoid indefinitely persisting stale data.
func (u *utxoNursery) incubator(newBlockChan *chainntnfs.BlockEpochEvent) {

	defer u.wg.Done()
	defer newBlockChan.Cancel()

	for {
		select {
		case epoch, ok := <-newBlockChan.Epochs:
			// If the epoch channel has been closed, then the
			// ChainNotifier is exiting which means the daemon is
			// as well. Therefore, we exit early also in order to
			// ensure the daemon shuts down gracefully, yet
			// swiftly.
			if !ok {
				return
			}

			// TODO(roasbeef): if the BlockChainIO is rescanning
			// will give stale data

			// A new block has just been connected to the main
			// chain, which means we might be able to graduate crib
			// or kindergarten outputs at this height. This involves
			// broadcasting any presigned htlc timeout txns, as well
			// as signing and broadcasting a sweep txn that spends
			// from all kindergarten outputs at this height.
			height := uint32(epoch.Height)

			u.mu.Lock()
			err := u.graduateClass(height)
			u.mu.Unlock()

			if err != nil {
				utxnLog.Errorf("error while graduating "+
					"class at height %d: %v", height, err)
			}

		case <-u.quit:
			return
		}
	}
}

// contractMaturityReport is a report that details the maturity progress of a
// particular force closed contract.
type contractMaturityReport struct {
	// chanPoint is the channel point of the original contract that is now
	// awaiting maturity within the utxoNursery.
	chanPoint wire.OutPoint

	// limboBalance is the total number of frozen coins within this
	// contract.
	limboBalance btcutil.Amount

	// confirmationHeight is the block height that this output originally
	// confirmed at.
	confirmationHeight uint32

	// maturityRequirement is the input age required for this output to
	// reach maturity.
	maturityRequirement uint32

	// maturityHeight is the absolute block height that this output will
	// mature at.
	maturityHeight uint32
}

// NurseryReport attempts to return a nursery report stored for the target
// outpoint. A nursery report details the maturity/sweeping progress for a
// contract that was previously force closed. If a report entry for the target
// chanPoint is unable to be constructed, then an error will be returned.
func (u *utxoNursery) NurseryReport(
	chanPoint *wire.OutPoint) (*contractMaturityReport, error) {

	utxnLog.Infof("NurseryReport: building nursery report for channel %v",
		chanPoint)

	var report *contractMaturityReport
	if err := u.cfg.Store.ForChanOutputs(chanPoint, func(k, v []byte) error {
		var prefix [4]byte
		copy(prefix[:], k[:4])

		switch string(prefix[:]) {
		case string(psclPrefix), string(kndrPrefix):

			// information for this immature output.
			var kid kidOutput
			kidReader := bytes.NewReader(v)
			err := kid.Decode(kidReader)
			if err != nil {
				return err
			}

			utxnLog.Infof("NurseryReport: found kid output: %v",
				kid.OutPoint())

			// TODO(roasbeef): should actually be list of outputs
			report = &contractMaturityReport{
				chanPoint:           *chanPoint,
				limboBalance:        kid.Amount(),
				maturityRequirement: kid.BlocksToMaturity(),
			}

			// If the confirmation height is set, then this means the
			// contract has been confirmed, and we know the final maturity
			// height.
			if kid.ConfHeight() != 0 {
				report.confirmationHeight = kid.ConfHeight()
				report.maturityHeight = (kid.BlocksToMaturity() +
					kid.ConfHeight())
			}

		case string(cribPrefix):
			utxnLog.Infof("NurseryReport: found crib output: %x", k[4:])

		case string(gradPrefix):
			utxnLog.Infof("NurseryReport: found grad output: %x", k[4:])

		default:
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return report, nil
}

// waitForEnrollment watches for the confirmation of an htlc timeout
// transaction, and attempts to move the htlc output from the crib bucket to the
// kindergarten bucket upon success.
func (u *utxoNursery) waitForEnrollment(baby *babyOutput,
	confChan *chainntnfs.ConfirmationEvent) {

	defer u.wg.Done()

	select {
	case txConfirmation, ok := <-confChan.Confirmed:
		if !ok {
			utxnLog.Errorf("Notification chan "+
				"closed, can't advance baby output %v",
				baby.OutPoint())
			return
		}

		baby.SetConfHeight(txConfirmation.BlockHeight)

	case <-u.quit:
		return
	}

	// TODO(conner): add retry logic?

	u.mu.Lock()
	defer u.mu.Unlock()

	err := u.cfg.Store.CribToKinder(baby)
	if err != nil {
		utxnLog.Errorf("Unable to move htlc output from "+
			"crib to kindergarten bucket: %v", err)
		return
	}

	utxnLog.Infof("Htlc output %v promoted to "+
		"kindergarten", baby.OutPoint())
}

// waitForPromotion is intended to be run as a goroutine that will wait until a
// channel force close commitment transaction has been included in a confirmed
// block. Once the transaction has been confirmed (as reported by the Chain
// Notifier), waitForPromotion will delete the output from the "preschool"
// database bucket and atomically add it to the "kindergarten" database bucket.
// This is the second step in the output incubation process.
func (u *utxoNursery) waitForPromotion(kid *kidOutput,
	confChan *chainntnfs.ConfirmationEvent) {

	defer u.wg.Done()

	select {
	case txConfirmation, ok := <-confChan.Confirmed:
		if !ok {
			utxnLog.Errorf("Notification chan "+
				"closed, can't advance output %v",
				kid.OutPoint())
			return
		}

		kid.SetConfHeight(txConfirmation.BlockHeight)

	case <-u.quit:
		return
	}

	// TODO(conner): add retry logic?

	u.mu.Lock()
	defer u.mu.Unlock()

	err := u.cfg.Store.PreschoolToKinder(kid)
	if err != nil {
		utxnLog.Errorf("Unable to move kid output "+
			"from preschool to kindergarten bucket: %v",
			err)
		return
	}

	utxnLog.Infof("Preschool output %v promoted to "+
		"kindergarten", kid.OutPoint())
}

// waitForGraduation watches for the confirmation of a sweep transaction
// containing a batch of kindergarten outputs. Once confirmation has been
// received, the nursery will mark those outputs as fully graduated, and proceed
// to mark any mature channels as fully closed in channeldb.
// NOTE(conner): this method MUST be called as a go routine.
func (u *utxoNursery) waitForGraduation(classHeight uint32, kgtnOutputs []kidOutput,
	confChan *chainntnfs.ConfirmationEvent) {

	defer u.wg.Done()

	select {
	case _, ok := <-confChan.Confirmed:
		if !ok {
			utxnLog.Errorf("Notification chan closed, can't"+
				" advance %v graduating outputs", len(kgtnOutputs))
			return
		}

	case <-u.quit:
		return
	}

	// TODO(conner): add retry logic?

	u.mu.Lock()
	defer u.mu.Unlock()

	// Mark the confirmed kindergarten outputs as graduated.
	if err := u.cfg.Store.GraduateKinder(classHeight, kgtnOutputs); err != nil {
		utxnLog.Errorf("Unable to award diplomas to %v"+
			"graduating outputs: %v", len(kgtnOutputs), err)
		return
	}

	utxnLog.Infof("Graduated %d kindergarten outputs from height %d",
		len(kgtnOutputs), classHeight)

	// Iterate over the kid outputs and construct a set of all channel
	// points to which they belong.
	var possibleCloses = make(map[wire.OutPoint]struct{})
	for _, kid := range kgtnOutputs {
		possibleCloses[*kid.OriginChanPoint()] = struct{}{}

	}

	// Attempt to close each channel, only doing so if all of the channel's
	// outputs have been graduated.
	for chanPoint := range possibleCloses {
		if err := u.closeAndRemoveIfMature(&chanPoint); err != nil {
			utxnLog.Errorf("Failed to close and remove channel %v", chanPoint)
			return
		}
	}

	if err := u.cfg.Store.TryFinalizeClass(classHeight); err != nil {
		utxnLog.Errorf("Attempt to finalize height %d failed", classHeight)
		return
	}

	lastHeight, _ := u.cfg.Store.LastFinalizedHeight()

	utxnLog.Errorf("Successfully finalized height %d of %d", lastHeight, classHeight)
}

// closeAndRemoveIfMature removes a particular channel from the channel index
// if and only if all of its outputs have been marked graduated. If the channel
// still has ungraduated outputs, the method will succeed without altering the
// database state.
func (u *utxoNursery) closeAndRemoveIfMature(chanPoint *wire.OutPoint) error {
	isMature, err := u.cfg.Store.IsMatureChannel(chanPoint)
	if err == ErrContractNotFound {
		return nil
	} else if err != nil {
		utxnLog.Errorf("Unable to determine maturity of "+
			"channel %v", chanPoint)
		return err
	}

	// Nothing to do if we are still incubating.
	if !isMature {
		return nil
	}

	// Now that the sweeping transaction has been broadcast, for
	// each of the immature outputs, we'll mark them as being fully
	// closed within the database.
	err = u.cfg.DB.MarkChanFullyClosed(chanPoint)
	if err != nil {
		utxnLog.Errorf("Unable to mark channel %v as fully "+
			"closed: %v", chanPoint, err)
		return err
	}

	utxnLog.Infof("Marked channel %v as fully closed", chanPoint)

	if err := u.cfg.Store.RemoveChannel(chanPoint); err != nil {
		utxnLog.Errorf("Unable to remove channel %v from "+
			"nursery store: %v", chanPoint, err)
		return err
	}

	utxnLog.Infof("Removed channel %v from nursery store", chanPoint)

	return nil
}

// newSweepPkScript creates a new public key script which should be used to
// sweep any time-locked, or contested channel funds into the wallet.
// Specifically, the script generated is a version 0,
// pay-to-witness-pubkey-hash (p2wkh) output.
func newSweepPkScript(wallet lnwallet.WalletController) ([]byte, error) {
	sweepAddr, err := wallet.NewAddress(lnwallet.WitnessPubKey, false)
	if err != nil {
		return nil, err
	}

	return txscript.PayToAddrScript(sweepAddr)
}

// CsvSpendableOutput is a SpendableOutput that contains all of the information
// necessary to construct, sign, and sweep an output locked with a CSV delay.
type CsvSpendableOutput interface {
	SpendableOutput

	// ConfHeight returns the height at which this output was confirmed.
	// A zero value indicates that the output has not been confirmed.
	ConfHeight() uint32

	// SetConfHeight marks the height at which the output is confirmed in
	// the chain.
	SetConfHeight(height uint32)

	// BlocksToMaturity returns the relative timelock, as a number of
	// blocks, that must be built on top of the confirmation height before
	// the output can be spent.
	BlocksToMaturity() uint32

	// OriginChanPoint returns the outpoint of the channel from which this
	// output is derived.
	OriginChanPoint() *wire.OutPoint
}

// babyOutput is an HTLC output that is in the earliest stage of upbringing.
// Each babyOutput carries a presigned timeout transction, which should be
// broadcast at the appropriate CLTV expiry, and its future kidOutput self. If
// all goes well, and the timeout transaction is successfully confirmed, the
// the now-mature kidOutput will be unwrapped and continue its journey through
// the nursery.
type babyOutput struct {
	// expiry is the absolute block height at which the timeoutTx should be
	// broadcast to the network.
	expiry uint32

	// timeoutTx is a fully-signed transaction that, upon confirmation,
	// transitions the htlc into the delay+claim stage.
	timeoutTx *wire.MsgTx

	// kidOutput represents the CSV output to be swept after the timeoutTx has
	// been broadcast and confirmed.
	kidOutput
}

// makeBabyOutput constructs a baby output the wraps a future kidOutput. The
// provided sign descriptors and witness types will be used once the output
// reaches the delay and claim stage.
func makeBabyOutput(outpoint, originChanPoint *wire.OutPoint,
	blocksToMaturity uint32, witnessType lnwallet.WitnessType,
	htlcResolution *lnwallet.OutgoingHtlcResolution) babyOutput {

	kid := makeKidOutput(outpoint, originChanPoint,
		blocksToMaturity, witnessType,
		&htlcResolution.SweepSignDesc)

	return babyOutput{
		kidOutput: kid,
		expiry:    htlcResolution.Expiry,
		timeoutTx: htlcResolution.SignedTimeoutTx,
	}
}

// Encode writes the baby output to the given io.Writer.
func (bo *babyOutput) Encode(w io.Writer) error {
	var scratch [4]byte
	byteOrder.PutUint32(scratch[:], bo.expiry)
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := bo.timeoutTx.Serialize(w); err != nil {
		return err
	}

	return bo.kidOutput.Encode(w)
}

// Decode reconstructs a baby output using the provided io.Reader.
func (bo *babyOutput) Decode(r io.Reader) error {
	var scratch [4]byte
	if _, err := r.Read(scratch[:]); err != nil {
		return err
	}
	bo.expiry = byteOrder.Uint32(scratch[:])

	bo.timeoutTx = new(wire.MsgTx)
	if err := bo.timeoutTx.Deserialize(r); err != nil {
		return err
	}

	return bo.kidOutput.Decode(r)
}

// kidOutput represents an output that's waiting for a required blockheight
// before its funds will be available to be moved into the user's wallet.  The
// struct includes a WitnessGenerator closure which will be used to generate
// the witness required to sweep the output once it's mature.
//
// TODO(roasbeef): rename to immatureOutput?
type kidOutput struct {
	breachedOutput

	originChanPoint wire.OutPoint

	// TODO(roasbeef): using block timeouts everywhere currently, will need
	// to modify logic later to account for MTP based timeouts.
	blocksToMaturity uint32
	confHeight       uint32
}

func makeKidOutput(outpoint, originChanPoint *wire.OutPoint,
	blocksToMaturity uint32, witnessType lnwallet.WitnessType,
	signDescriptor *lnwallet.SignDescriptor) kidOutput {

	return kidOutput{
		breachedOutput: makeBreachedOutput(
			outpoint, witnessType, signDescriptor,
		),
		originChanPoint:  *originChanPoint,
		blocksToMaturity: blocksToMaturity,
	}
}

func (k *kidOutput) OriginChanPoint() *wire.OutPoint {
	return &k.originChanPoint
}

func (k *kidOutput) BlocksToMaturity() uint32 {
	return k.blocksToMaturity
}

func (k *kidOutput) SetConfHeight(height uint32) {
	k.confHeight = height
}

func (k *kidOutput) ConfHeight() uint32 {
	return k.confHeight
}

// Encode converts a KidOutput struct into a form suitable for on-disk database
// storage. Note that the signDescriptor struct field is included so that the
// output's witness can be generated by createSweepTx() when the output becomes
// spendable.
func (k *kidOutput) Encode(w io.Writer) error {
	var scratch [8]byte
	byteOrder.PutUint64(scratch[:], uint64(k.Amount()))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := writeOutpoint(w, k.OutPoint()); err != nil {
		return err
	}
	if err := writeOutpoint(w, k.OriginChanPoint()); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], k.BlocksToMaturity())
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], k.ConfHeight())
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	byteOrder.PutUint16(scratch[:2], uint16(k.WitnessType()))
	if _, err := w.Write(scratch[:2]); err != nil {
		return err
	}

	return lnwallet.WriteSignDescriptor(w, k.SignDesc())
}

// Decode takes a byte array representation of a kidOutput and converts it to an
// struct. Note that the witnessFunc method isn't added during deserialization
// and must be added later based on the value of the witnessType field.
func (k *kidOutput) Decode(r io.Reader) error {
	var scratch [8]byte

	if _, err := r.Read(scratch[:]); err != nil {
		return err
	}
	k.amt = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	if err := readOutpoint(io.LimitReader(r, 40), &k.outpoint); err != nil {
		return err
	}

	err := readOutpoint(io.LimitReader(r, 40), &k.originChanPoint)
	if err != nil {
		return err
	}

	if _, err := r.Read(scratch[:4]); err != nil {
		return err
	}
	k.blocksToMaturity = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(scratch[:4]); err != nil {
		return err
	}
	k.confHeight = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(scratch[:2]); err != nil {
		return err
	}
	k.witnessType = lnwallet.WitnessType(byteOrder.Uint16(scratch[:2]))

	return lnwallet.ReadSignDescriptor(r, &k.signDesc)
}

// TODO(bvu): copied from channeldb, remove repetition
func writeOutpoint(w io.Writer, o *wire.OutPoint) error {
	// TODO(roasbeef): make all scratch buffers on the stack
	scratch := make([]byte, 4)

	// TODO(roasbeef): write raw 32 bytes instead of wasting the extra
	// byte.
	if err := wire.WriteVarBytes(w, 0, o.Hash[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch, o.Index)
	_, err := w.Write(scratch)
	return err
}

// TODO(bvu): copied from channeldb, remove repetition
func readOutpoint(r io.Reader, o *wire.OutPoint) error {
	scratch := make([]byte, 4)

	txid, err := wire.ReadVarBytes(r, 0, 32, "prevout")
	if err != nil {
		return err
	}
	copy(o.Hash[:], txid)

	if _, err := r.Read(scratch); err != nil {
		return err
	}
	o.Index = byteOrder.Uint32(scratch)

	return nil
}

func writeTxOut(w io.Writer, txo *wire.TxOut) error {
	scratch := make([]byte, 8)

	byteOrder.PutUint64(scratch, uint64(txo.Value))
	if _, err := w.Write(scratch); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, txo.PkScript); err != nil {
		return err
	}

	return nil
}

func readTxOut(r io.Reader, txo *wire.TxOut) error {
	scratch := make([]byte, 8)

	if _, err := r.Read(scratch); err != nil {
		return err
	}
	txo.Value = int64(byteOrder.Uint64(scratch))

	pkScript, err := wire.ReadVarBytes(r, 0, 80, "pkScript")
	if err != nil {
		return err
	}
	txo.PkScript = pkScript

	return nil
}

// Compile-time constraint to ensure kidOutput and babyOutpt implement the
// CsvSpendableOutput interface.
var _ CsvSpendableOutput = (*kidOutput)(nil)
var _ CsvSpendableOutput = (*babyOutput)(nil)
