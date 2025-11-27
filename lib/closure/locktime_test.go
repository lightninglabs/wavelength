package closure

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestBIP68Sequence_Encoding verifies that BIP68Sequence correctly encodes
// relative locktimes for both blocks and seconds.
func TestBIP68Sequence_Encoding(t *testing.T) {
	t.Parallel()

	t.Run("block-based encoding", func(t *testing.T) {
		t.Parallel()

		testCases := []struct {
			blocks   uint32
			expected uint32
		}{
			{blocks: 1, expected: 1},
			{blocks: 10, expected: 10},
			{blocks: 144, expected: 144},     // ~1 day
			{blocks: 1008, expected: 1008},   // ~1 week
			{blocks: 65535, expected: 65535}, // Maximum blocks
		}

		for _, tc := range testCases {
			locktime := RelativeLocktime{
				Type:  LocktimeTypeBlock,
				Value: tc.blocks,
			}

			seq, err := BIP68Sequence(locktime)
			require.NoError(t, err)

			// Block-based should not have the time flag set.
			require.Zero(t, seq&SEQUENCE_LOCKTIME_TYPE_FLAG,
				"block-based should not have time flag")

			// Value should be in the lower 16 bits.
			require.Equal(t, tc.expected, seq&SEQUENCE_LOCKTIME_MASK)
		}
	})

	t.Run("seconds-based encoding", func(t *testing.T) {
		t.Parallel()

		testCases := []struct {
			seconds  uint32
			expected uint32
		}{
			// Minimum unit (512s = 1 unit).
			{seconds: 512, expected: 1},
			// 1024s = 2 units.
			{seconds: 1024, expected: 2},
			// ~1 hour (rounded down to multiple of 512).
			{seconds: 3600 - 3600%512, expected: (3600 - 3600%512) / 512},
			// ~1 day (rounded down to multiple of 512).
			{seconds: 86400 - 86400%512, expected: (86400 - 86400%512) / 512},
		}

		for _, tc := range testCases {
			locktime := RelativeLocktime{
				Type:  LocktimeTypeSecond,
				Value: tc.seconds,
			}

			seq, err := BIP68Sequence(locktime)
			require.NoError(t, err)

			// Seconds-based should have the time flag set.
			require.NotZero(t, seq&SEQUENCE_LOCKTIME_TYPE_FLAG,
				"seconds-based should have time flag")

			// Value should be seconds / 512 in the lower 16 bits.
			require.Equal(t, tc.expected, seq&SEQUENCE_LOCKTIME_MASK)
		}
	})

	t.Run("seconds must be multiple of 512", func(t *testing.T) {
		t.Parallel()

		locktime := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 513, // Not a multiple of 512.
		}

		_, err := BIP68Sequence(locktime)
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be a multiple of 512")
	})

	t.Run("seconds maximum value", func(t *testing.T) {
		t.Parallel()

		// SECONDS_MAX = 65535 * 512 = 33553920 seconds.
		locktime := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: SECONDS_MAX,
		}

		seq, err := BIP68Sequence(locktime)
		require.NoError(t, err)
		require.NotZero(t, seq)

		// Exceeding max should fail.
		locktime.Value = SECONDS_MAX + 512
		_, err = BIP68Sequence(locktime)
		require.Error(t, err)
		require.Contains(t, err.Error(), "seconds too large")
	})
}

// TestBIP68DecodeSequenceFromBytes verifies that BIP68DecodeSequenceFromBytes
// correctly decodes BIP68 sequences from script number bytes.
func TestBIP68DecodeSequenceFromBytes(t *testing.T) {
	t.Parallel()

	t.Run("decode block-based sequence", func(t *testing.T) {
		t.Parallel()

		testCases := []struct {
			name   string
			blocks uint32
		}{
			{"small blocks", 10},
			{"medium blocks", 144},
			{"large blocks", 1000},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				// Encode first.
				original := RelativeLocktime{
					Type:  LocktimeTypeBlock,
					Value: tc.blocks,
				}
				seq, err := BIP68Sequence(original)
				require.NoError(t, err)

				// Convert to script bytes.
				seqBytes := encodeSequenceToBytes(seq)

				// Decode.
				decoded, err := BIP68DecodeSequenceFromBytes(seqBytes)
				require.NoError(t, err)
				require.NotNil(t, decoded)

				require.Equal(t, LocktimeTypeBlock, decoded.Type)
				require.Equal(t, tc.blocks, decoded.Value)
			})
		}
	})

	t.Run("decode seconds-based sequence", func(t *testing.T) {
		t.Parallel()

		testCases := []struct {
			name    string
			seconds uint32
		}{
			{"minimum seconds", 512},
			{"hour of seconds", 3584}, // 7 * 512
			{"day of seconds", 86016}, // 168 * 512
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				// Encode first.
				original := RelativeLocktime{
					Type:  LocktimeTypeSecond,
					Value: tc.seconds,
				}
				seq, err := BIP68Sequence(original)
				require.NoError(t, err)

				// Convert to script bytes.
				seqBytes := encodeSequenceToBytes(seq)

				// Decode.
				decoded, err := BIP68DecodeSequenceFromBytes(seqBytes)
				require.NoError(t, err)
				require.NotNil(t, decoded)

				require.Equal(t, LocktimeTypeSecond, decoded.Type)
				require.Equal(t, tc.seconds, decoded.Value)
			})
		}
	})

	t.Run("roundtrip encoding/decoding", func(t *testing.T) {
		t.Parallel()

		testCases := []RelativeLocktime{
			{Type: LocktimeTypeBlock, Value: 1},
			{Type: LocktimeTypeBlock, Value: 100},
			{Type: LocktimeTypeBlock, Value: 65535},
			{Type: LocktimeTypeSecond, Value: 512},
			{Type: LocktimeTypeSecond, Value: 1024},
			{Type: LocktimeTypeSecond, Value: 512 * 100},
		}

		for _, original := range testCases {
			seq, err := BIP68Sequence(original)
			require.NoError(t, err)

			seqBytes := encodeSequenceToBytes(seq)
			decoded, err := BIP68DecodeSequenceFromBytes(seqBytes)
			require.NoError(t, err)
			require.NotNil(t, decoded)

			require.Equal(t, original.Type, decoded.Type)
			require.Equal(t, original.Value, decoded.Value)
		}
	})
}

// TestBIP68DecodeSequence verifies that BIP68DecodeSequence correctly decodes
// BIP68 sequences from uint32 values.
func TestBIP68DecodeSequence(t *testing.T) {
	t.Parallel()

	t.Run("decode disabled sequence", func(t *testing.T) {
		t.Parallel()

		// Sequence with disable flag set.
		seq := uint32(wire.SequenceLockTimeDisabled)

		decoded, disabled := BIP68DecodeSequence(seq)
		require.True(t, disabled)
		require.Nil(t, decoded)
	})

	t.Run("decode block-based sequence", func(t *testing.T) {
		t.Parallel()

		// Block-based sequence (no time flag).
		seq := uint32(100)

		decoded, disabled := BIP68DecodeSequence(seq)
		require.False(t, disabled)
		require.NotNil(t, decoded)
		require.Equal(t, LocktimeTypeBlock, decoded.Type)
		require.Equal(t, uint32(100), decoded.Value)
	})

	t.Run("decode seconds-based sequence", func(t *testing.T) {
		t.Parallel()

		// Seconds-based sequence (time flag set).
		// 10 units * 512 seconds/unit = 5120 seconds.
		seq := uint32(wire.SequenceLockTimeIsSeconds | 10)

		decoded, disabled := BIP68DecodeSequence(seq)
		require.False(t, disabled)
		require.NotNil(t, decoded)
		require.Equal(t, LocktimeTypeSecond, decoded.Type)
		// Note: BIP68DecodeSequence subtracts 1 from the time calculation.
		require.Equal(t, uint32(10*512-1), decoded.Value)
	})
}

// TestRelativeLocktime_Comparison verifies the comparison methods of
// RelativeLocktime work correctly.
func TestRelativeLocktime_Comparison(t *testing.T) {
	t.Parallel()

	t.Run("Compare returns correct ordering", func(t *testing.T) {
		t.Parallel()

		small := RelativeLocktime{Type: LocktimeTypeSecond, Value: 512}
		medium := RelativeLocktime{Type: LocktimeTypeSecond, Value: 1024}
		large := RelativeLocktime{Type: LocktimeTypeSecond, Value: 2048}

		// Small < Medium.
		require.Equal(t, -1, small.Compare(medium))

		// Medium > Small.
		require.Equal(t, 1, medium.Compare(small))

		// Equal values (comparing to self should return 0).
		mediumCopy := medium
		require.Equal(t, 0, medium.Compare(mediumCopy))

		// Large > Small.
		require.Equal(t, 1, large.Compare(small))
	})

	t.Run("LessThan returns correct result", func(t *testing.T) {
		t.Parallel()

		small := RelativeLocktime{Type: LocktimeTypeSecond, Value: 512}
		large := RelativeLocktime{Type: LocktimeTypeSecond, Value: 2048}

		require.True(t, small.LessThan(large))
		require.False(t, large.LessThan(small))
		require.False(t, small.LessThan(small))
	})

	t.Run("compare blocks to seconds", func(t *testing.T) {
		t.Parallel()

		// 1 block = ~10 minutes = 600 seconds.
		oneBlock := RelativeLocktime{Type: LocktimeTypeBlock, Value: 1}
		fiveMinutes := RelativeLocktime{Type: LocktimeTypeSecond, Value: 512}

		// 1 block (600s) > 512s.
		require.Equal(t, 1, oneBlock.Compare(fiveMinutes))
		require.False(t, oneBlock.LessThan(fiveMinutes))
		require.True(t, fiveMinutes.LessThan(oneBlock))
	})

	t.Run("equivalent block and seconds values", func(t *testing.T) {
		t.Parallel()

		// 1 block = 600 seconds (10 minutes).
		oneBlock := RelativeLocktime{
			Type: LocktimeTypeBlock, Value: 1,
		}
		sixHundredSeconds := RelativeLocktime{
			Type: LocktimeTypeSecond, Value: 600,
		}

		// They should be equal when compared via Seconds().
		require.Equal(t, oneBlock.Seconds(), sixHundredSeconds.Seconds())
		require.Equal(t, 0, oneBlock.Compare(sixHundredSeconds))
	})
}

// TestRelativeLocktime_Seconds verifies that the Seconds() method correctly
// converts relative locktimes to seconds.
func TestRelativeLocktime_Seconds(t *testing.T) {
	t.Parallel()

	t.Run("seconds type returns value directly", func(t *testing.T) {
		t.Parallel()

		locktime := RelativeLocktime{
			Type:  LocktimeTypeSecond,
			Value: 3600,
		}

		require.Equal(t, int64(3600), locktime.Seconds())
	})

	t.Run("blocks type converts to seconds", func(t *testing.T) {
		t.Parallel()

		// 1 block = SECONDS_PER_BLOCK = 600 seconds.
		locktime := RelativeLocktime{
			Type:  LocktimeTypeBlock,
			Value: 1,
		}

		require.Equal(t, int64(SECONDS_PER_BLOCK), locktime.Seconds())

		// 144 blocks = 1 day = 86400 seconds.
		locktime.Value = 144
		require.Equal(t, int64(144*SECONDS_PER_BLOCK), locktime.Seconds())
	})
}

// TestAbsoluteLocktime_IsSeconds verifies that AbsoluteLocktime.IsSeconds()
// correctly identifies seconds vs block-based locktimes.
func TestAbsoluteLocktime_IsSeconds(t *testing.T) {
	t.Parallel()

	t.Run("values >= 500000000 are seconds", func(t *testing.T) {
		t.Parallel()

		// Exactly at threshold.
		locktime := AbsoluteLocktime(500000000)
		require.True(t, locktime.IsSeconds())

		// Above threshold.
		locktime = AbsoluteLocktime(500000001)
		require.True(t, locktime.IsSeconds())

		locktime = AbsoluteLocktime(1700000000) // Unix timestamp.
		require.True(t, locktime.IsSeconds())
	})

	t.Run("values < 500000000 are blocks", func(t *testing.T) {
		t.Parallel()

		// Just below threshold.
		locktime := AbsoluteLocktime(499999999)
		require.False(t, locktime.IsSeconds())

		// Typical block heights.
		locktime = AbsoluteLocktime(800000)
		require.False(t, locktime.IsSeconds())

		locktime = AbsoluteLocktime(1)
		require.False(t, locktime.IsSeconds())

		locktime = AbsoluteLocktime(0)
		require.False(t, locktime.IsSeconds())
	})
}

// encodeSequenceToBytes is a helper that encodes a uint32 sequence into
// script number bytes for testing purposes.
func encodeSequenceToBytes(seq uint32) []byte {
	if seq == 0 {
		return []byte{0}
	}

	// Encode as little-endian with proper sign handling.
	result := make([]byte, 0, 5)
	neg := seq < 0

	absValue := seq
	if neg {
		absValue = -seq
	}

	for absValue > 0 {
		result = append(result, byte(absValue&0xff))
		absValue >>= 8
	}

	// If the high bit is set, we need to add another byte to indicate sign.
	if result[len(result)-1]&0x80 != 0 {
		if neg {
			result = append(result, 0x80)
		} else {
			result = append(result, 0x00)
		}
	} else if neg {
		result[len(result)-1] |= 0x80
	}

	return result
}
