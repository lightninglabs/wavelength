package closure

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// createTestKey creates a deterministic key for testing.
func createTestKey(index int32) *btcec.PublicKey {
	_, pubKey := btcec.PrivKeyFromBytes([]byte{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(index + 1),
	})
	return pubKey
}

// TestValidate_ServerKeyRequirement verifies that the server/signer key must
// be present in all forfeit (collaborative) closures.
func TestValidate_ServerKeyRequirement(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)
	otherKey := createTestKey(3)

	minLocktime := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 512,
	}
	exitDelay := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 1024,
	}

	t.Run("valid MultisigClosure with owner and signer", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.NoError(t, err)
	})

	t.Run("invalid MultisigClosure missing signer key", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, otherKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "signer pubkey not found")
	})

	t.Run("valid CLTVMultisigClosure with signer key", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
				&CLTVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
					},
					Locktime: AbsoluteLocktime(500000001),
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.NoError(t, err)
	})

	t.Run("invalid CLTVMultisigClosure missing signer key", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
				&CLTVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey},
					},
					Locktime: AbsoluteLocktime(500000001),
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "signer pubkey not found")
	})

	t.Run("valid ConditionMultisigClosure with signer key", func(t *testing.T) {
		// Simple condition: OP_TRUE
		condition := []byte{0x51}

		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
				&ConditionMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
					},
					Condition: condition,
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.NoError(t, err)
	})

	t.Run("valid 3-of-3 MultisigClosure with signer key", func(t *testing.T) {
		// Signer key present among 3 keys.
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{
						ownerKey, signerKey, otherKey,
					},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.NoError(t, err)
	})
}

// TestValidate_LocktimeMinimum verifies that exit closures must meet the
// minimum locktime requirement.
func TestValidate_LocktimeMinimum(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	minLocktime := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 1024,
	}

	t.Run("valid exit delay >= minimum", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey: ownerKey,
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeSecond,
						Value: 2048,
					},
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.NoError(t, err)
	})

	t.Run("valid exit delay exactly equal to minimum", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: minLocktime,
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.NoError(t, err)
	})

	t.Run("invalid exit delay less than minimum", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey: ownerKey,
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeSecond,
						Value: 512,
					},
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exit delay is too short")
	})

	t.Run("multiple exits - smallest must meet minimum", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				// Long delay - meets minimum.
				&CSVSigClosure{
					PubKey: ownerKey,
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeSecond,
						Value: 4096,
					},
				},
				// Short delay - below minimum.
				&CSVSigClosure{
					PubKey: ownerKey,
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeSecond,
						Value: 512,
					},
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exit delay is too short")
	})

	t.Run("CSVMultisigClosure respects minimum locktime", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey},
					},
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeSecond,
						Value: 512,
					},
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exit delay is too short")
	})
}

// TestValidate_BlockTypeRestriction verifies that block-type locktimes can
// be disallowed via the blockTypeAllowed parameter.
func TestValidate_BlockTypeRestriction(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	minLocktime := RelativeLocktime{
		Type:  LocktimeTypeBlock,
		Value: 10,
	}

	t.Run("block-based CSV allowed when blockTypeAllowed=true", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey: ownerKey,
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeBlock,
						Value: 100,
					},
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.NoError(t, err)
	})

	t.Run("block-based CSV rejected when blockTypeAllowed=false", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey: ownerKey,
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeBlock,
						Value: 100,
					},
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "CSV block type not allowed")
	})

	t.Run("seconds-based CSV allowed when blockTypeAllowed=false", func(t *testing.T) {
		minLocktimeSeconds := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 512,
		}

		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey: ownerKey,
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeSecond,
						Value: 1024,
					},
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktimeSeconds, false)
		require.NoError(t, err)
	})

	t.Run("block-based CLTV rejected when blockTypeAllowed=false", func(t *testing.T) {
		minLocktimeSeconds := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 512,
		}

		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey: ownerKey,
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeSecond,
						Value: 1024,
					},
				},
				&CLTVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
					},
					// Block-based (< 500000000).
					Locktime: AbsoluteLocktime(1000),
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktimeSeconds, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "CLTV block type not allowed")
	})

	t.Run("seconds-based CLTV allowed when blockTypeAllowed=false", func(t *testing.T) {
		minLocktimeSeconds := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 512,
		}

		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey: ownerKey,
					Locktime: RelativeLocktime{
						Type:  LocktimeTypeSecond,
						Value: 1024,
					},
				},
				&CLTVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
					},
					// Seconds-based (>= 500000000).
					Locktime: AbsoluteLocktime(500000001),
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktimeSeconds, false)
		require.NoError(t, err)
	})
}

// TestValidate_NoExitClosures verifies that VTXOs with no exit closures
// pass validation (special case where ErrNoExitLeaf is ignored).
func TestValidate_NoExitClosures(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	minLocktime := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 512,
	}

	t.Run("forfeit-only VTXO passes validation", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.NoError(t, err)
	})
}

// TestValidate_MultipleForfeitClosures verifies that ALL forfeit closures
// must contain the server key, not just some of them.
func TestValidate_MultipleForfeitClosures(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)
	otherKey := createTestKey(3)

	minLocktime := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 512,
	}
	exitDelay := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 1024,
	}

	t.Run("all forfeit closures have signer key", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
				&CLTVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
					},
					Locktime: AbsoluteLocktime(500000001),
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.NoError(t, err)
	})

	t.Run("one forfeit closure missing signer key fails", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
				// This one has signer key.
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
				// This one does NOT have signer key.
				&CLTVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey, otherKey},
					},
					Locktime: AbsoluteLocktime(500000001),
				},
			},
		}

		err := vtxoScript.Validate(signerKey, minLocktime, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "signer pubkey not found")
	})
}

// TestExitClosures_FilteringLogic verifies that ExitClosures returns only
// CSV-based closures (CSVSigClosure and CSVMultisigClosure).
func TestExitClosures_FilteringLogic(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	exitDelay := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 1024,
	}

	vtxoScript := &TapscriptsVtxoScript{
		Closures: []Closure{
			&CSVSigClosure{
				PubKey:   ownerKey,
				Locktime: exitDelay,
			},
			&CSVMultisigClosure{
				MultisigClosure: MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey},
				},
				Locktime: exitDelay,
			},
			&MultisigClosure{
				PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
			},
			&CLTVMultisigClosure{
				MultisigClosure: MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
				Locktime: AbsoluteLocktime(500000001),
			},
		},
	}

	exits := vtxoScript.ExitClosures()
	require.Len(t, exits, 2)

	// Verify types.
	_, ok := exits[0].(*CSVSigClosure)
	require.True(t, ok, "first exit should be CSVSigClosure")

	_, ok = exits[1].(*CSVMultisigClosure)
	require.True(t, ok, "second exit should be CSVMultisigClosure")
}

// TestForfeitClosures_FilteringLogic verifies that ForfeitClosures returns
// only collaborative closures (MultisigClosure, CLTVMultisigClosure,
// ConditionMultisigClosure).
func TestForfeitClosures_FilteringLogic(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	exitDelay := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 1024,
	}

	condition := []byte{0x51} // OP_TRUE

	vtxoScript := &TapscriptsVtxoScript{
		Closures: []Closure{
			&CSVSigClosure{
				PubKey:   ownerKey,
				Locktime: exitDelay,
			},
			&MultisigClosure{
				PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
			},
			&CLTVMultisigClosure{
				MultisigClosure: MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
				Locktime: AbsoluteLocktime(500000001),
			},
			&ConditionMultisigClosure{
				MultisigClosure: MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
				Condition: condition,
			},
		},
	}

	forfeits := vtxoScript.ForfeitClosures()
	require.Len(t, forfeits, 3)

	// Verify types.
	_, ok := forfeits[0].(*MultisigClosure)
	require.True(t, ok, "first forfeit should be MultisigClosure")

	_, ok = forfeits[1].(*CLTVMultisigClosure)
	require.True(t, ok, "second forfeit should be CLTVMultisigClosure")

	_, ok = forfeits[2].(*ConditionMultisigClosure)
	require.True(t, ok, "third forfeit should be ConditionMultisigClosure")
}

// TestSmallestExitDelay verifies that SmallestExitDelay correctly finds the
// minimum exit delay across all CSV closures.
func TestSmallestExitDelay(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	t.Run("single exit closure returns its delay", func(t *testing.T) {
		exitDelay := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 1024,
		}

		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		smallest, err := vtxoScript.SmallestExitDelay()
		require.NoError(t, err)
		require.Equal(t, exitDelay, *smallest)
	})

	t.Run("multiple exit closures returns smallest", func(t *testing.T) {
		shortDelay := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 512,
		}
		longDelay := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 4096,
		}

		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: longDelay,
				},
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: shortDelay,
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		smallest, err := vtxoScript.SmallestExitDelay()
		require.NoError(t, err)
		require.Equal(t, shortDelay, *smallest)
	})

	t.Run("no exit closures returns ErrNoExitLeaf", func(t *testing.T) {
		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		smallest, err := vtxoScript.SmallestExitDelay()
		require.Error(t, err)
		require.ErrorIs(t, err, ErrNoExitLeaf)
		require.Nil(t, smallest)
	})

	t.Run("mixed CSV types finds smallest", func(t *testing.T) {
		shortDelay := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 512,
		}
		longDelay := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 4096,
		}

		vtxoScript := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: longDelay,
				},
				&CSVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey},
					},
					Locktime: shortDelay,
				},
				&MultisigClosure{
					PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
				},
			},
		}

		smallest, err := vtxoScript.SmallestExitDelay()
		require.NoError(t, err)
		require.Equal(t, shortDelay, *smallest)
	})
}

// TestEncodeDecodeRoundtrip verifies that encoding and decoding a VTXO script
// preserves all closure data.
func TestEncodeDecodeRoundtrip(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	exitDelay := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 1024,
	}

	t.Run("default VTXO script roundtrip", func(t *testing.T) {
		original := NewDefaultVtxoScript(ownerKey, signerKey, exitDelay)

		encoded, err := original.Encode()
		require.NoError(t, err)
		require.Len(t, encoded, 2)

		decoded := &TapscriptsVtxoScript{}
		err = decoded.Decode(encoded)
		require.NoError(t, err)
		require.Len(t, decoded.Closures, 2)

		// Verify exit closure.
		exitClosure, ok := decoded.Closures[0].(*CSVSigClosure)
		require.True(t, ok)
		require.Equal(t, exitDelay, exitClosure.Locktime)

		// Verify collab closure.
		collabClosure, ok := decoded.Closures[1].(*MultisigClosure)
		require.True(t, ok)
		require.Len(t, collabClosure.PubKeys, 2)
	})

	t.Run("custom VTXO script roundtrip", func(t *testing.T) {
		original := &TapscriptsVtxoScript{
			Closures: []Closure{
				&CSVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
						Type:    MultisigTypeChecksig,
					},
					Locktime: exitDelay,
				},
				&CLTVMultisigClosure{
					MultisigClosure: MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerKey, signerKey},
						Type:    MultisigTypeChecksig,
					},
					Locktime: AbsoluteLocktime(500000001),
				},
			},
		}

		encoded, err := original.Encode()
		require.NoError(t, err)
		require.Len(t, encoded, 2)

		decoded := &TapscriptsVtxoScript{}
		err = decoded.Decode(encoded)
		require.NoError(t, err)
		require.Len(t, decoded.Closures, 2)

		// Verify first closure is CSVMultisigClosure.
		_, ok := decoded.Closures[0].(*CSVMultisigClosure)
		require.True(t, ok)

		// Verify second closure is CLTVMultisigClosure.
		_, ok = decoded.Closures[1].(*CLTVMultisigClosure)
		require.True(t, ok)
	})
}

// TestParseVtxoScript verifies the ParseVtxoScript helper function.
func TestParseVtxoScript(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	exitDelay := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 1024,
	}

	t.Run("valid scripts", func(t *testing.T) {
		original := NewDefaultVtxoScript(ownerKey, signerKey, exitDelay)
		encoded, err := original.Encode()
		require.NoError(t, err)

		parsed, err := ParseVtxoScript(encoded)
		require.NoError(t, err)
		require.Len(t, parsed.Closures, 2)
	})

	t.Run("empty scripts returns error", func(t *testing.T) {
		parsed, err := ParseVtxoScript([]string{})
		require.Error(t, err)
		require.Nil(t, parsed)
	})

	t.Run("invalid hex returns error", func(t *testing.T) {
		parsed, err := ParseVtxoScript([]string{"not_valid_hex"})
		require.Error(t, err)
		require.Nil(t, parsed)
	})
}

// TestTapTree verifies that TapTree correctly builds a taproot tree from
// the closures.
func TestTapTree(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	exitDelay := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 1024,
	}

	vtxoScript := NewDefaultVtxoScript(ownerKey, signerKey, exitDelay)

	taprootKey, tree, err := vtxoScript.TapTree()
	require.NoError(t, err)
	require.NotNil(t, taprootKey)
	require.NotNil(t, tree)

	// Verify we can get leaves.
	leaves := tree.GetLeaves()
	require.Len(t, leaves, 2)

	// Verify we can get spend info for each closure.
	for i := range vtxoScript.Closures {
		spendInfo, err := vtxoScript.GetSpendInfo(i)
		require.NoError(t, err)
		require.NotEmpty(t, spendInfo.Script)
		require.NotEmpty(t, spendInfo.ControlBlock)
	}
}

// TestGetSpendInfo verifies the GetSpendInfo method.
func TestGetSpendInfo(t *testing.T) {
	t.Parallel()

	ownerKey := createTestKey(1)
	signerKey := createTestKey(2)

	exitDelay := RelativeLocktime{
		Type:  LocktimeTypeSecond,
		Value: 1024,
	}

	vtxoScript := NewDefaultVtxoScript(ownerKey, signerKey, exitDelay)

	t.Run("valid index returns spend info", func(t *testing.T) {
		spendInfo, err := vtxoScript.GetSpendInfo(0)
		require.NoError(t, err)
		require.NotNil(t, spendInfo)
		require.NotEmpty(t, spendInfo.Script)
		require.NotEmpty(t, spendInfo.ControlBlock)
	})

	t.Run("negative index returns error", func(t *testing.T) {
		spendInfo, err := vtxoScript.GetSpendInfo(-1)
		require.Error(t, err)
		require.Nil(t, spendInfo)
		require.Contains(t, err.Error(), "out of bounds")
	})

	t.Run("index out of bounds returns error", func(t *testing.T) {
		spendInfo, err := vtxoScript.GetSpendInfo(10)
		require.Error(t, err)
		require.Nil(t, spendInfo)
		require.Contains(t, err.Error(), "out of bounds")
	})
}
