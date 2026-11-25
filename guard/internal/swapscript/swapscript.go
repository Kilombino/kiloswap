// Package swapscript rebuilds a Boltz (v2, taproot) swap output from data
// the guard trusts, so that nothing the swap backend writes in its database
// can make the guard pay or lock against an output it did not check:
//
//	internal key  MuSig2 KeyAgg of [server, user], unsorted (boltz-core)
//	claim leaf    [OP_SIZE 32 OP_EQUALVERIFY] OP_HASH160 <ripemd160(hash)>
//	              OP_EQUALVERIFY <claim x-only> OP_CHECKSIG
//	refund leaf   <refund x-only> OP_CHECKSIGVERIFY <timeout> OP_CHECKLOCKTIMEVERIFY
//
// Reverse swaps (we lock, the user claims) carry the OP_SIZE check;
// submarine swaps (the user locks, we claim) do not.
package swapscript

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // the script's hash
)

// Kind is the swap's direction.
type Kind int

const (
	// Submarine: the user locks on-chain, we claim with the preimage.
	Submarine Kind = iota
	// Reverse: we lock on-chain, the user claims with the preimage.
	Reverse
)

// Params is everything an output depends on.
type Params struct {
	Kind        Kind
	Server      *btcec.PublicKey // our key: claim (submarine) or refund (reverse)
	User        *btcec.PublicKey // theirs: refund (submarine) or claim (reverse)
	PaymentHash []byte           // sha256 of the preimage
	Timeout     uint32           // absolute block height
}

// Output is a rebuilt swap output.
type Output struct {
	ClaimLeaf, RefundLeaf txscript.TapLeaf
	InternalKey           *btcec.PublicKey
	OutputKey             *btcec.PublicKey
	PkScript              []byte
	tree                  *txscript.IndexedTapScriptTree
}

// Build rebuilds the output.
func Build(p Params) (*Output, error) {
	if p.Server == nil || p.User == nil {
		return nil, errors.New("both keys are required")
	}
	if len(p.PaymentHash) != 32 {
		return nil, errors.New("a 32-byte payment hash is required")
	}
	if p.Timeout == 0 || p.Timeout >= 500_000_000 {
		return nil, fmt.Errorf("timeout %d is not a block height", p.Timeout)
	}
	claimKey, refundKey := p.Server, p.User
	if p.Kind == Reverse {
		claimKey, refundKey = p.User, p.Server
	}
	r := ripemd160.New()
	r.Write(p.PaymentHash)
	h160 := r.Sum(nil)

	cb := txscript.NewScriptBuilder()
	if p.Kind == Reverse {
		cb.AddOp(txscript.OP_SIZE).AddInt64(32).AddOp(txscript.OP_EQUALVERIFY)
	}
	cb.AddOp(txscript.OP_HASH160).AddData(h160).AddOp(txscript.OP_EQUALVERIFY).
		AddData(schnorr.SerializePubKey(claimKey)).AddOp(txscript.OP_CHECKSIG)
	claim, err := cb.Script()
	if err != nil {
		return nil, err
	}
	refund, err := txscript.NewScriptBuilder().
		AddData(schnorr.SerializePubKey(refundKey)).AddOp(txscript.OP_CHECKSIGVERIFY).
		AddInt64(int64(p.Timeout)).AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).Script()
	if err != nil {
		return nil, err
	}
	agg, _, _, err := musig2.AggregateKeys([]*btcec.PublicKey{p.Server, p.User}, false)
	if err != nil {
		return nil, err
	}
	o := &Output{
		ClaimLeaf:   txscript.NewBaseTapLeaf(claim),
		RefundLeaf:  txscript.NewBaseTapLeaf(refund),
		InternalKey: agg.FinalKey,
	}
	o.tree = txscript.AssembleTaprootScriptTree(o.ClaimLeaf, o.RefundLeaf)
	root := o.tree.RootNode.TapHash()
	o.OutputKey = txscript.ComputeTaprootOutputKey(o.InternalKey, root[:])
	o.PkScript, err = txscript.PayToTaprootScript(o.OutputKey)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// ControlBlock is the control block spending through leaf.
func (o *Output) ControlBlock(leaf txscript.TapLeaf) ([]byte, error) {
	idx, ok := o.tree.LeafProofIndex[leaf.TapHash()]
	if !ok {
		return nil, errors.New("leaf not in the tree")
	}
	cb := o.tree.LeafMerkleProofs[idx].ToControlBlock(o.InternalKey)
	return cb.ToBytes()
}

// Matches tells whether pkScript is this output.
func (o *Output) Matches(pkScript []byte) bool { return bytes.Equal(o.PkScript, pkScript) }

// PreimageFromClaim returns the preimage if witness spends out through its
// claim leaf: [signature, preimage, claim leaf script, control block].
func (o *Output) PreimageFromClaim(witness [][]byte, paymentHash []byte) ([]byte, bool) {
	if len(witness) < 4 {
		return nil, false
	}
	n := len(witness)
	if len(witness[n-1]) > 0 && witness[n-1][0] == txscript.TaprootAnnexTag {
		witness = witness[:n-1]
		n--
	}
	if n != 4 || !bytes.Equal(witness[2], o.ClaimLeaf.Script) {
		return nil, false
	}
	pre := witness[1]
	h := sha256.Sum256(pre)
	if len(pre) != 32 || !bytes.Equal(h[:], paymentHash) {
		return nil, false
	}
	return pre, true
}

func sha256sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

// SpendsLeaf tells whether a script-path witness spends through leaf (its
// next-to-last item is the leaf's script).
func (o *Output) SpendsLeaf(witness [][]byte, leaf txscript.TapLeaf) bool {
	n := len(witness)
	return n >= 3 && bytes.Equal(witness[n-2], leaf.Script)
}
