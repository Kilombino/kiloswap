// Package keys holds the swaps' server key. It lives only in the guard:
// the swap backend is given the extended public key at the same path, so
// it can put our public keys in new swaps but can sign nothing.
package keys

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/tyler-smith/go-bip39"
)

// Keys derives the swaps' keys at Path/index (Boltz: m/0/0).
type Keys struct {
	account *hdkeychain.ExtendedKey // at Path
	Path    string
}

// FromMnemonicFile reads a BIP39 mnemonic (the backend's former seed.dat).
func FromMnemonicFile(file, path string) (*Keys, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	words := strings.Join(strings.Fields(string(b)), " ")
	if !bip39.IsMnemonicValid(words) {
		return nil, errors.New("not a valid BIP39 mnemonic")
	}
	master, err := hdkeychain.NewMaster(bip39.NewSeed(words, ""), &chaincfg.MainNetParams)
	if err != nil {
		return nil, err
	}
	acct, err := derivePath(master, path)
	if err != nil {
		return nil, err
	}
	return &Keys{account: acct, Path: path}, nil
}

func derivePath(k *hdkeychain.ExtendedKey, path string) (*hdkeychain.ExtendedKey, error) {
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] != "m" {
		return nil, fmt.Errorf("path %q must start with m", path)
	}
	for _, p := range parts[1:] {
		hard := strings.HasSuffix(p, "'") || strings.HasSuffix(p, "h")
		n, err := strconv.ParseUint(strings.TrimRight(p, "'h"), 10, 31)
		if err != nil {
			return nil, fmt.Errorf("path %q: %w", path, err)
		}
		i := uint32(n)
		if hard {
			i += hdkeychain.HardenedKeyStart
		}
		if k, err = k.Derive(i); err != nil {
			return nil, err
		}
	}
	return k, nil
}

func (k *Keys) child(index uint32) (*hdkeychain.ExtendedKey, error) {
	if index >= hdkeychain.HardenedKeyStart {
		return nil, errors.New("index out of range")
	}
	return k.account.Derive(index)
}

// Private is the server key of the swap at index.
func (k *Keys) Private(index uint32) (*btcec.PrivateKey, error) {
	c, err := k.child(index)
	if err != nil {
		return nil, err
	}
	return c.ECPrivKey()
}

// Public is the server public key of the swap at index.
func (k *Keys) Public(index uint32) (*btcec.PublicKey, error) {
	c, err := k.child(index)
	if err != nil {
		return nil, err
	}
	return c.ECPubKey()
}

// AccountXpub is what the backend gets instead of the mnemonic.
func (k *Keys) AccountXpub() (string, error) {
	n, err := k.account.Neuter()
	if err != nil {
		return "", err
	}
	return n.String(), nil
}
