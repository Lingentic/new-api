package relay

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayTaskVideoSecondsComputesQuotaBySecond(t *testing.T) {
	gin.SetMode(gin.TestMode)

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"happyhorse-1.1-r2v":"video_seconds"}`,
	}))
	require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(`{
		"happyhorse-1.1-r2v": {
			"720p": {"default": 0.9}
		}
	}`))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("group", "default")
	ctx.Set("task_request", relaycommon.TaskSubmitReq{
		Model:    "happyhorse-1.1-r2v",
		Duration: 5,
		Metadata: map[string]any{
			"resolution": "720P",
		},
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: "happyhorse-1.1-r2v",
		UserGroup:       "default",
		UsingGroup:      "default",
	}
	info.PriceData.GroupRatioInfo.GroupRatio = 1

	require.NoError(t, applyVideoSecondsBilling(ctx, info))
	expectedQuota := int(0.9 * 5 * common.QuotaPerUnit * 1.0)
	require.Equal(t, expectedQuota, info.PriceData.Quota)
	require.Equal(t, "720p", info.PriceData.VideoSecondsTier)
}

func TestRelayTaskVideoSecondsAddsInputImageCostAfterDuration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	savedVideoPrices := ratio_setting.VideoSecondsPrice2JSONString()
	savedInputPrices := ratio_setting.ImageInputPrice2JSONString()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(savedVideoPrices))
		require.NoError(t, ratio_setting.UpdateImageInputPriceByJSONString(savedInputPrices))
	})
	require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(`{
		"happyhorse-1.1-r2v":{"720p":{"default":0.1}}
	}`))
	require.NoError(t, ratio_setting.UpdateImageInputPriceByJSONString(`{
		"happyhorse-1.1-r2v":{"default":0.02}
	}`))

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("task_request", relaycommon.TaskSubmitReq{
		Model: "happyhorse-1.1-r2v", Duration: 5,
		Metadata: map[string]any{"resolution": "720p"},
	})
	info := &relaycommon.RelayInfo{OriginModelName: "happyhorse-1.1-r2v"}
	info.PriceData.UsePrice = true
	info.PriceData.GroupRatioInfo.GroupRatio = 1
	helper.ApplyImageInputPricing(info, &info.PriceData, []string{"default", "default"})

	require.NoError(t, applyVideoSecondsBilling(ctx, info))
	assert.Equal(t, common.QuotaFromFloat((0.1*5+0.02*2)*common.QuotaPerUnit), info.PriceData.Quota)
}

func TestRecalcQuotaFromRatiosDoesNotMultiplyInputImageCost(t *testing.T) {
	groupRatio := 2.0
	baseQuota := common.QuotaFromFloat(1 * common.QuotaPerUnit * groupRatio)
	inputImageQuota := common.QuotaFromFloat(0.5 * common.QuotaPerUnit * groupRatio)
	info := &relaycommon.RelayInfo{}
	info.PriceData.Quota = baseQuota*3 + inputImageQuota
	info.PriceData.InputImageCost = 0.5
	info.PriceData.GroupRatioInfo.GroupRatio = groupRatio
	info.PriceData.AddOtherRatio("duration", 3)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{"duration": 4})

	require.True(t, ok)
	assert.Equal(t, baseQuota*4+inputImageQuota, quota)
}

func TestRelayTaskVideoSecondsUsesMappedOpenRouterModelPricing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	savedPrices := ratio_setting.VideoSecondsPrice2JSONString()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(savedPrices))
	})
	require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(`{
		"alibaba/happyhorse-1.1": {
			"1080p": {"default": 0.1278}
		}
	}`))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("group", "default")
	ctx.Set("task_request", relaycommon.TaskSubmitReq{
		Model:   "happyhorse-1.1-t2v",
		Prompt:  "a running horse",
		Seconds: "5",
		Size:    "1080p",
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: "happyhorse-1.1-t2v",
		UserGroup:       "default",
		UsingGroup:      "default",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "alibaba/happyhorse-1.1",
			IsModelMapped:     true,
		},
	}
	info.PriceData.GroupRatioInfo.GroupRatio = 1

	assert.Equal(t, billing_setting.BillingModeVideoSeconds,
		billing_setting.ResolveBillingMode(info.OriginModelName, info.GetUpstreamModelName()))
	require.NoError(t, applyVideoSecondsBilling(ctx, info))
	assert.Equal(t, common.QuotaFromFloat(0.1278*5*common.QuotaPerUnit), info.PriceData.Quota)
	assert.Equal(t, 0.1278, info.PriceData.VideoSecondsUnitPrice)
	assert.Equal(t, "1080p", info.PriceData.VideoSecondsTier)
	assert.Equal(t, 5, info.PriceData.VideoDurationSeconds)
}

func TestRelayTaskVideoSecondsAddsMiniMaxReferenceImagePrice(t *testing.T) {
	gin.SetMode(gin.TestMode)

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"minimax/hailuo-3":"video_seconds"}`,
	}))
	require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(`{
		"minimax/hailuo-3": {
			"2k": {"default": 0.13, "silent": 0.13, "reference_image": 0.04}
		}
	}`))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("group", "default")
	ctx.Set("task_request", relaycommon.TaskSubmitReq{
		Model:    "minimax/hailuo-3",
		Duration: 5,
		Images: []string{
			"https://example.com/first.png",
			"https://example.com/last.png",
		},
		Metadata: map[string]any{
			"reference_images": []string{
				"https://example.com/reference-1.png",
				"https://example.com/reference-2.png",
				"https://example.com/reference-3.png",
				"https://example.com/reference-4.png",
				"https://example.com/reference-5.png",
				"https://example.com/reference-6.png",
				"https://example.com/reference-7.png",
			},
		},
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: "minimax/hailuo-3",
		UserGroup:       "default",
		UsingGroup:      "default",
	}
	info.PriceData.GroupRatioInfo.GroupRatio = 1

	require.NoError(t, applyVideoSecondsBilling(ctx, info))
	assert.Equal(t, 0.08, info.PriceData.VideoFixedPrice)
	expectedQuota := int((0.13*5 + 0.04*2) * common.QuotaPerUnit)
	assert.Equal(t, expectedQuota, info.PriceData.Quota)
}

func TestRelayTaskVideoSecondsFailsWhenTierPriceMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"happyhorse-1.1-r2v":"video_seconds"}`,
	}))
	require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(`{
		"happyhorse-1.1-r2v": {
			"1080p": {"default": 1.2}
		}
	}`))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("group", "default")
	ctx.Set("task_request", relaycommon.TaskSubmitReq{
		Model:    "happyhorse-1.1-r2v",
		Duration: 5,
		Metadata: map[string]any{
			"resolution": "720P",
		},
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: "happyhorse-1.1-r2v",
		UserGroup:       "default",
		UsingGroup:      "default",
	}
	info.PriceData.GroupRatioInfo.GroupRatio = 1

	err := applyVideoSecondsBilling(ctx, info)
	require.Error(t, err)
	require.Equal(t, billing_setting.BillingModeVideoSeconds, billing_setting.GetBillingMode(info.OriginModelName))
}

func TestRelayTaskVideoSecondsUsesMappedUpstreamBillingConverter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"kling-v3-video-generation":"video_seconds"}`,
	}))
	require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(`{
		"kling-v3-video-generation": {
			"720p": {"default": 0.9, "silent": 0.6}
		}
	}`))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("group", "default")
	ctx.Set("task_request", relaycommon.TaskSubmitReq{
		Model:   "kling-v3-video-generation",
		Seconds: "5",
		Size:    "720p",
		Metadata: map[string]any{
			"aspect_ratio": "16:9",
			"audio":        false,
			"watermark":    false,
		},
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: "kling-v3-video-generation",
		UserGroup:       "default",
		UsingGroup:      "default",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "kling/kling-v3-video-generation",
			IsModelMapped:     true,
		},
	}
	info.PriceData.GroupRatioInfo.GroupRatio = 1

	require.NoError(t, applyVideoSecondsBilling(ctx, info))
	assert.Equal(t, int(0.6*5*common.QuotaPerUnit), info.PriceData.Quota)
	assert.Equal(t, "720p", info.PriceData.VideoSecondsTier)
	assert.Equal(t, 5, info.PriceData.VideoDurationSeconds)
	require.NotNil(t, info.PriceData.VideoAudioEnabled)
	assert.False(t, *info.PriceData.VideoAudioEnabled)
}

func TestRelayTaskVideoSecondsUsesDoubaoConverterWhenUpstreamModelIsOpaque(t *testing.T) {
	gin.SetMode(gin.TestMode)

	savedConfig := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		savedConfig[key] = value
		return nil
	}))
	savedVideoPrices := ratio_setting.VideoSecondsPrice2JSONString()
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(savedConfig))
	})
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(savedVideoPrices))
	})

	const modelName = "doubao-seedance-2-0-260128"
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"doubao-seedance-2-0-260128":"video_seconds"}`,
	}))
	require.NoError(t, ratio_setting.UpdateVideoSecondsPriceByJSONString(`{
		"doubao-seedance-2-0-260128": {
			"480p": {"default": 0.25}
		}
	}`))

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("group", "default")
	ctx.Set("task_request", relaycommon.TaskSubmitReq{
		Model: modelName, Resolution: "480p", Duration: 7,
	})
	info := &relaycommon.RelayInfo{
		OriginModelName: modelName,
		UserGroup:       "default",
		UsingGroup:      "default",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "ep-20260914-opaque",
			IsModelMapped:     true,
		},
	}
	info.PriceData.GroupRatioInfo.GroupRatio = 1

	assert.Equal(t, billing_setting.BillingModeVideoSeconds,
		billing_setting.ResolveBillingMode(modelName, info.GetUpstreamModelName()))
	require.NoError(t, applyVideoSecondsBilling(ctx, info))
	assert.Equal(t, common.QuotaFromFloat(0.25*7*common.QuotaPerUnit), info.PriceData.Quota)
	assert.Equal(t, 0.25, info.PriceData.VideoSecondsUnitPrice)
	assert.Equal(t, "480p", info.PriceData.VideoSecondsTier)
	assert.Equal(t, 7, info.PriceData.VideoDurationSeconds)
}
