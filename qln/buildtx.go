package qln

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/fastsha256"
	"github.com/lightningnetwork/lnd/lnutil"

	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/txsort"
)

// SimpleCloseTx produces a close tx based on the current state.
// The PKH addresses are my refund base with their r-elkrem point, and
// their refund base with my r-elkrem point.  "Their" point means they have
// the point but not the scalar.
func (q *Qchan) SimpleCloseTx() (*wire.MsgTx, error) {
	// sanity checks
	if q == nil || q.State == nil {
		return nil, fmt.Errorf("SimpleCloseTx: nil chan / state")
	}
	fee := int64(5000) // fixed fee for now (on both sides)

	// get final elkrem points; both R, theirs and mine
	theirElkPointR, err := q.ElkPoint(false, false, q.State.StateIdx)
	if err != nil {
		fmt.Printf("SimpleCloseTx: can't generate elkpoint.")
		return nil, err
	}

	// my pub is my base and "their" elk point which I have the scalar for
	myRefundPub := lnutil.AddPubs(q.MyRefundPub, theirElkPointR)
	// their pub is their base and "my" elk point (which they gave me)
	theirRefundPub := lnutil.AddPubs(q.TheirRefundPub, q.State.ElkPointR)

	// make my output
	myScript := DirectWPKHScript(myRefundPub)
	myOutput := wire.NewTxOut(q.State.MyAmt-fee, myScript)
	// make their output
	theirScript := DirectWPKHScript(theirRefundPub)
	theirOutput := wire.NewTxOut((q.Value-q.State.MyAmt)-fee, theirScript)

	// make tx with these outputs
	tx := wire.NewMsgTx()
	tx.AddTxOut(myOutput)
	tx.AddTxOut(theirOutput)
	// add channel outpoint as txin
	tx.AddTxIn(wire.NewTxIn(&q.Op, nil, nil))
	// sort and return
	txsort.InPlaceSort(tx)
	return tx, nil
}

// BuildStateTx constructs and returns a state tx.  As simple as I can make it.
// This func just makes the tx with data from State in ram, and HAKD key arg
// Delta should always be 0 when making this tx.
// It decides whether to make THEIR tx or YOUR tx based on the HAKD pubkey given --
// if it's zero, then it makes their transaction (for signing onlu)
// If it's full, it makes your transaction (for verification in most cases,
// but also for signing when breaking the channel)
// Index is used to set nlocktime for state hints.
// fee and op_csv timeout are currently hardcoded, make those parameters later.
// also returns the script preimage for later spending.
func (q *Qchan) BuildStateTx(mine bool) (*wire.MsgTx, error) {
	if q == nil {
		return nil, fmt.Errorf("BuildStateTx: nil chan")
	}
	// sanity checks
	s := q.State // use it a lot, make shorthand variable
	if s == nil {
		return nil, fmt.Errorf("channel (%d,%d) has no state", q.KeyGen.Step[3], q.KeyGen.Step[4])
	}
	// if delta is non-zero, something is wrong.
	if s.Delta != 0 {
		return nil, fmt.Errorf(
			"BuildStateTx: delta is %d (expect 0)", s.Delta)
	}
	var fancyAmt, pkhAmt int64   // output amounts
	var revPub, timePub [33]byte // pubkeys
	var pkhPub [33]byte          // the simple output's pub key hash
	fee := int64(5000)           // fixed fee for now
	delay := uint16(5)           // fixed CSV delay for now
	// delay is super short for testing.

	// Both received and self-generated elkpoints are needed
	// Here generate the elk point we give them (we know the scalar; they don't)
	theirElkPointR, theirElkPointT, err := q.MakeTheirCurElkPoints()
	if err != nil {
		return nil, err
	}
	// the PKH clear refund also has elkrem points added to mask the PKH.
	// this changes the txouts at each state to blind sorceror better.
	if mine { // build MY tx (to verify) (unless breaking)
		// My tx that I store.  They get funds unencumbered.
		// SH pubkeys are our base points plus the elk point we give them
		revPub = lnutil.AddPubs(q.TheirHAKDBase, theirElkPointR)
		timePub = lnutil.AddPubs(q.MyHAKDBase, theirElkPointT)

		pkhPub = lnutil.AddPubs(q.TheirRefundPub, s.ElkPointR) // my received elkpoint
		pkhAmt = (q.Value - s.MyAmt) - fee
		fancyAmt = s.MyAmt - fee

		fmt.Printf("\t refund base %x, elkpointR %x\n", q.TheirRefundPub, s.ElkPointR)
	} else { // build THEIR tx (to sign)
		// Their tx that they store.  I get funds unencumbered.

		// SH pubkeys are our base points plus the received elk point
		revPub = lnutil.AddPubs(q.MyHAKDBase, s.ElkPointR)
		timePub = lnutil.AddPubs(q.TheirHAKDBase, s.ElkPointT)
		fancyAmt = (q.Value - s.MyAmt) - fee

		// PKH output
		pkhPub = lnutil.AddPubs(q.MyRefundPub, theirElkPointR) // their (sent) elk point
		pkhAmt = s.MyAmt - fee
		fmt.Printf("\trefund base %x, elkpointR %x\n", q.MyRefundPub, theirElkPointR)
	}

	// now that everything is chosen, build fancy script and pkh script
	fancyScript, _ := CommitScript2(revPub, timePub, delay)
	pkhScript := DirectWPKHScript(pkhPub) // p2wpkh-ify

	fmt.Printf("> made SH script, state %d\n", s.StateIdx)
	fmt.Printf("\t revPub %x timeout pub %x \n", revPub, timePub)
	fmt.Printf("\t script %x ", fancyScript)

	fancyScript = P2WSHify(fancyScript) // p2wsh-ify

	fmt.Printf("\t scripthash %x\n", fancyScript)

	// create txouts by assigning amounts
	outFancy := wire.NewTxOut(fancyAmt, fancyScript)
	outPKH := wire.NewTxOut(pkhAmt, pkhScript)

	fmt.Printf("\tcombined refund %x, pkh %x\n", pkhPub, outPKH.PkScript)

	// make a new tx
	tx := wire.NewMsgTx()
	// add txouts
	tx.AddTxOut(outFancy)
	tx.AddTxOut(outPKH)
	// add unsigned txin
	tx.AddTxIn(wire.NewTxIn(&q.Op, nil, nil))
	// set index hints
	var x uint64
	if s.StateIdx > 0 { // state 0 and 1 can't use xor'd elkrem... fix this?
		x = q.GetElkZeroOffset()
		if x >= 1<<48 {
			return nil, fmt.Errorf("BuildStateTx elkrem error, x= %x", x)
		}
	}
	SetStateIdxBits(tx, s.StateIdx, x)

	// sort outputs
	txsort.InPlaceSort(tx)
	return tx, nil
}

func DirectWPKHScript(pub [33]byte) []byte {
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_0).AddData(btcutil.Hash160(pub[:]))
	b, _ := builder.Script()
	return b
}

// CommitScript2 doesn't use hashes, but a modified pubkey.
// To spend from it, push your sig.  If it's time-based,
// you have to set the txin's sequence.
func CommitScript2(RKey, TKey [33]byte, delay uint16) ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	builder.AddOp(txscript.OP_DUP)
	builder.AddData(RKey[:])
	builder.AddOp(txscript.OP_CHECKSIG)

	builder.AddOp(txscript.OP_NOTIF)

	builder.AddData(TKey[:])
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(int64(delay))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// FundMultiOut creates a TxOut for the funding transaction.
// Give it the two pubkeys and it'll give you the p2sh'd txout.
// You don't have to remember the p2sh preimage, as long as you remember the
// pubkeys involved.
func FundTxOut(pubA, puB [33]byte, amt int64) (*wire.TxOut, error) {
	if amt < 0 {
		return nil, fmt.Errorf("Can't create FundTx script with negative coins")
	}
	scriptBytes, _, err := FundTxScript(pubA, puB)
	if err != nil {
		return nil, err
	}
	scriptBytes = P2WSHify(scriptBytes)

	return wire.NewTxOut(amt, scriptBytes), nil
}

// FundMultiPre generates the non-p2sh'd multisig script for 2 of 2 pubkeys.
// useful for making transactions spending the fundtx.
// returns a bool which is true if swapping occurs.
func FundTxScript(aPub, bPub [33]byte) ([]byte, bool, error) {
	var swapped bool
	if bytes.Compare(aPub[:], bPub[:]) == -1 { // swap to sort pubkeys if needed
		aPub, bPub = bPub, aPub
		swapped = true
	}
	bldr := txscript.NewScriptBuilder()
	// Require 1 signatures, either key// so from both of the pubkeys
	bldr.AddOp(txscript.OP_2)
	// add both pubkeys (sorted)
	bldr.AddData(aPub[:])
	bldr.AddData(bPub[:])
	// 2 keys total.  In case that wasn't obvious.
	bldr.AddOp(txscript.OP_2)
	// Good ol OP_CHECKMULTISIG.  Don't forget the zero!
	bldr.AddOp(txscript.OP_CHECKMULTISIG)
	// get byte slice
	pre, err := bldr.Script()
	return pre, swapped, err
}

// the scriptsig to put on a P2SH input.  Sigs need to be in order!
func SpendMultiSigWitStack(pre, sigA, sigB []byte) [][]byte {

	witStack := make([][]byte, 4)

	witStack[0] = nil // it's not an OP_0 !!!! argh!
	witStack[1] = sigA
	witStack[2] = sigB
	witStack[3] = pre

	return witStack
}

func P2WSHify(scriptBytes []byte) []byte {
	bldr := txscript.NewScriptBuilder()
	bldr.AddOp(txscript.OP_0)
	wsh := fastsha256.Sum256(scriptBytes)
	bldr.AddData(wsh[:])
	b, _ := bldr.Script() // ignore script errors
	return b
}
