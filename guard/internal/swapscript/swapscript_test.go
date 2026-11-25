package swapscript

import (
	"encoding/hex"
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"

	"github.com/Kilombino/kiloswap/guard/internal/keys"
)

// Swaps the regtest lab's backend created (seed in LAB_SEED): the guard
// must rebuild exactly the address the backend gave the user.
var labSwaps = []struct {
	kind    Kind
	index   uint32
	user    string
	hash    string
	timeout uint32
	address string
}{
	{Submarine, 107, "0202ee23d70958a8e660de2a96adeeebe82ba60887cb8456eabad240715851e8b8", "337b4953d5f1265ce0f55758633878841730d184c95d0f20deef2874ef36639e", 15079, "bcrt1pv3w033z3xh80lar8t8ffgp6smqrhd52cs8yrfefrn8ttfnjr40aqgye5jq"},
	{Submarine, 106, "02dbc718400a2602c33d25430b981b83b98c403c2aeef2f90be82dfd4412e428bf", "1067e2b7d491211e3bf5ca2927b38c8577ff596464987218d9599d78672e7177", 14069, "bcrt1p079xtujgceql95dm00zta79mjr0tuelm73x0vchgjtsgdvcftjgsvcze8p"},
	{Reverse, 109, "0265ed8cf02bb0fb8b51bbc930f32c3be1bba753ab9e38ad6ba337940996d747e7", "3123b88cd24884d62706d7b41c86bfdc8db5a10d93feee4a173317afc6bc100b", 14219, "bcrt1p2agt4sq962zsh0j2g5r3plvu2tt99vvfk5dfygvu5aehw8avgdyqsm4fl5"},
	{Reverse, 108, "02100c53202f42e89e00cb2ea2b08eafd92e65aa3dcd7ce00432f596a86233b934", "f37501e088383fd3a1b77cf3408ae18895f92637aa562a7591d046256cadbc84", 14218, "bcrt1pn3edu9a5vtxasfw4f7vw7la8vcl3dsj3xqnuwarhl3c45gj8v5ssxee38a"},
}

func TestLabSwaps(t *testing.T) {
	seed := os.Getenv("LAB_SEED")
	if seed == "" {
		t.Skip("LAB_SEED: the lab backend's seed.dat")
	}
	k, err := keys.FromMnemonicFile(seed, "m/0/0")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range labSwaps {
		server, err := k.Public(s.index)
		if err != nil {
			t.Fatal(err)
		}
		ub, _ := hex.DecodeString(s.user)
		user, err := btcec.ParsePubKey(ub)
		if err != nil {
			t.Fatal(err)
		}
		h, _ := hex.DecodeString(s.hash)
		o, err := Build(Params{Kind: s.kind, Server: server, User: user, PaymentHash: h, Timeout: s.timeout})
		if err != nil {
			t.Fatal(err)
		}
		a, err := btcutil.DecodeAddress(s.address, &chaincfg.RegressionNetParams)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := txscript.PayToAddrScript(a)
		if !o.Matches(want) {
			t.Errorf("%s: rebuilt %x, want %x", s.address, o.PkScript, want)
		}
		// a wrong timeout, hash or kind must not match
		o2, _ := Build(Params{Kind: s.kind, Server: server, User: user, PaymentHash: h, Timeout: s.timeout + 1})
		if o2.Matches(want) {
			t.Errorf("%s: matched with another timeout", s.address)
		}
		o3, _ := Build(Params{Kind: 1 - s.kind, Server: server, User: user, PaymentHash: h, Timeout: s.timeout})
		if o3.Matches(want) {
			t.Errorf("%s: matched as the other kind", s.address)
		}
	}
}

func TestPreimageFromClaim(t *testing.T) {
	priv, _ := btcec.NewPrivateKey()
	user, _ := btcec.NewPrivateKey()
	pre := make([]byte, 32)
	pre[0] = 7
	h := sha256sum(pre)
	o, err := Build(Params{Kind: Reverse, Server: priv.PubKey(), User: user.PubKey(), PaymentHash: h, Timeout: 1000})
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := o.ControlBlock(o.ClaimLeaf)
	w := [][]byte{make([]byte, 64), pre, o.ClaimLeaf.Script, cb}
	if got, ok := o.PreimageFromClaim(w, h); !ok || hex.EncodeToString(got) != hex.EncodeToString(pre) {
		t.Fatal("preimage not found")
	}
	if _, ok := o.PreimageFromClaim([][]byte{make([]byte, 64), make([]byte, 32), o.ClaimLeaf.Script, cb}, h); ok {
		t.Fatal("a wrong preimage was taken")
	}
	if _, ok := o.PreimageFromClaim([][]byte{make([]byte, 64), pre, o.RefundLeaf.Script, cb}, h); ok {
		t.Fatal("a refund-leaf spend was taken as a claim")
	}
}
