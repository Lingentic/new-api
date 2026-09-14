package doubao

import (
	"testing"

	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoVideoBillingConverter(t *testing.T) {
	audio := false
	tests := []struct {
		name     string
		request  relaycommon.TaskSubmitReq
		tier     string
		duration int
		audio    bool
	}{
		{
			name: "canonical fields",
			request: relaycommon.TaskSubmitReq{
				Model: "doubao-seedance-2-0-fast-260128", Duration: 10, Resolution: "1080P", GenerateAudio: &audio,
			},
			tier: "1080p", duration: 10, audio: false,
		},
		{
			name: "seconds and metadata fallback",
			request: relaycommon.TaskSubmitReq{
				Model: "doubao-seedance-2-0-260128", Seconds: "8", Metadata: map[string]interface{}{"resolution": "4k", "audio": true},
			},
			tier: "4k", duration: 8, audio: true,
		},
		{
			name:    "defaults",
			request: relaycommon.TaskSubmitReq{Model: "doubao-seedance-2-0-260128"},
			tier:    "720p", duration: 5, audio: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, err := convertDoubaoVideoBillingParams(tt.request)
			require.NoError(t, err)
			assert.Equal(t, tt.tier, params.Tier)
			assert.Equal(t, tt.duration, params.DurationSeconds)
			assert.Equal(t, tt.audio, params.AudioEnabled)
		})
	}
}

func TestDoubaoVideoBillingConverterRejectsOversizedMetadataDuration(t *testing.T) {
	_, err := convertDoubaoVideoBillingParams(relaycommon.TaskSubmitReq{
		Model:    "doubao-seedance-2-0-260128",
		Metadata: map[string]interface{}{"durationSeconds": float64(relaycommon.MaxTaskDurationSeconds + 1)},
	})
	require.Error(t, err)
}

func TestDoubaoVideoBillingConverterIsRegistered(t *testing.T) {
	params, err := taskcommon.ConvertVideoBillingParams(nil, relaycommon.TaskSubmitReq{
		Model: "doubao-seedance-2-0-fast-260128", Duration: 5,
	})
	require.NoError(t, err)
	assert.Equal(t, "720p", params.Tier)
}

func TestDoubaoVideoBillingConverterFallsBackFromOpaqueUpstreamModel(t *testing.T) {
	info := &relaycommon.RelayInfo{
		OriginModelName: "doubao-seedance-2-0-260128",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "ep-20260914134700-example",
			IsModelMapped:     true,
		},
	}

	params, err := taskcommon.ConvertVideoBillingParams(info, relaycommon.TaskSubmitReq{
		Model:      "doubao-seedance-2-0-260128",
		Duration:   7,
		Resolution: "480p",
	})

	require.NoError(t, err)
	assert.Equal(t, "480p", params.Tier)
	assert.Equal(t, 7, params.DurationSeconds)
}

func TestDoubaoVideoBillingConverterFallsBackToOriginModel(t *testing.T) {
	info := &relaycommon.RelayInfo{
		OriginModelName: "doubao-seedance-2-0-260128",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "ep-20260914134700-example",
			IsModelMapped:     true,
		},
	}

	params, err := taskcommon.ConvertVideoBillingParams(info, relaycommon.TaskSubmitReq{
		Model:      "rewritten-request-model",
		Duration:   7,
		Resolution: "480p",
	})

	require.NoError(t, err)
	assert.Equal(t, "480p", params.Tier)
	assert.Equal(t, 7, params.DurationSeconds)
}
