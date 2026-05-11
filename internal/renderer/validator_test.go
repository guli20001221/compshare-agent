package renderer

import (
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/stretchr/testify/assert"
)

func TestValidateRenderedTextRejectsUnknownInstanceID(t *testing.T) {
	err := ValidateRenderedText(testResourceEnvelope(), "uhost-missing 正在运行")
	assert.Error(t, err)
}

func TestValidateRenderedTextRejectsAccountBillingClaims(t *testing.T) {
	for _, text := range []string{
		"本月总账单是 100 元",
		"账号总账单是 100 元",
		"余额是 10 元",
		"账号还剩 100 元",
		"账户还有多少钱",
		"账号用了多少钱",
		"balance is 10",
		"本月消费是 100 元",
		"本月扣费是 100 元",
		"账户本月费用为 100 元",
		"月度花费 100 元",
		"当月花了 100 元",
		"本月一共花了 100 元",
		"当月多少钱",
	} {
		t.Run(text, func(t *testing.T) {
			err := ValidateRenderedText(testResourceEnvelope(), text)
			assert.Error(t, err)
		})
	}
}

func TestValidateRenderedTextRejectsPercentWithoutMonitorFacts(t *testing.T) {
	env := envelope.Envelope{
		Kind: envelope.KindMonitorQuery,
		Constraints: envelope.Constraints{
			DoNotInventMetrics: true,
		},
	}
	err := ValidateRenderedText(env, "CPU 是 12%")
	assert.Error(t, err)
}

func TestValidateRenderedTextRejectsMonitorClaimsOutsideMonitorEnvelope(t *testing.T) {
	env := testResourceEnvelope()
	assert.Error(t, ValidateRenderedText(env, "CPU 使用率是 90%"))
	assert.Error(t, ValidateRenderedText(env, "GPU 使用率是 90"))
	assert.NoError(t, ValidateRenderedText(env, "CPU 是 2 核，内存是 4 GB"))
}

func TestValidateRenderedTextRejectsUngroundedMonitorPercent(t *testing.T) {
	env := envelope.Envelope{
		Kind: envelope.KindMonitorQuery,
		Facts: []envelope.Fact{{
			Key:    "GPU",
			Label:  "GPU 使用率",
			Value:  "87",
			Source: envelope.FactSourceAPI,
		}},
	}

	assert.NoError(t, ValidateRenderedText(env, "GPU 是 87%"))
	assert.NoError(t, ValidateRenderedText(env, "GPU 使用率是 87"))
	assert.Error(t, ValidateRenderedText(env, "GPU 是 99%"))
	assert.Error(t, ValidateRenderedText(env, "GPU 使用率是 99"))
	assert.NoError(t, ValidateRenderedText(env, "GPU 是 87％"))
	assert.Error(t, ValidateRenderedText(env, "GPU 是 99％"))
	assert.NoError(t, ValidateRenderedText(env, "GPU 是百分之87"))
	assert.Error(t, ValidateRenderedText(env, "GPU 是百分之99"))
	assert.Error(t, ValidateRenderedText(env, "CPU 是 87%"))
	assert.Error(t, ValidateRenderedText(env, "GPU 是 87%，CPU 是 87%"))
	assert.Error(t, ValidateRenderedText(env, "GPU 和 CPU 都是 87%"))
	assert.Error(t, ValidateRenderedText(env, "GPU 和处理器都是 87%"))

	cpuEnv := envelope.Envelope{
		Kind: envelope.KindMonitorQuery,
		Facts: []envelope.Fact{{
			Key:    "CPU",
			Label:  "CPU 使用率",
			Value:  "87",
			Source: envelope.FactSourceAPI,
		}},
	}
	assert.Error(t, ValidateRenderedText(cpuEnv, "CPU 和显卡都是 87%"))
}

func TestValidateRenderedTextRejectsUnknownInstanceLikeName(t *testing.T) {
	err := ValidateRenderedText(testResourceEnvelope(), "prod-db-01 当前运行中")
	assert.Error(t, err)
}

func TestValidateRenderedTextAllowsFactKeysAndLabels(t *testing.T) {
	env := testResourceEnvelope()
	env.Facts = append(env.Facts,
		envelope.Fact{Key: "gpu_type", Label: "GPU型号", Value: "4090", Source: envelope.FactSourceAPI},
		envelope.Fact{Key: "image_type", Label: "镜像类型", Value: "Ubuntu", Source: envelope.FactSourceAPI},
	)

	assert.NoError(t, ValidateRenderedText(env, "gpu_type 是 4090，image_type 是 Ubuntu"))
}

func TestValidateRenderedTextDoesNotTreatNonMetricNumbersAsPercents(t *testing.T) {
	env := envelope.Envelope{
		Kind: envelope.KindMonitorQuery,
		Facts: []envelope.Fact{{
			Key:    "sample_count",
			Value:  12,
			Source: envelope.FactSourceAPI,
		}},
	}

	assert.Error(t, ValidateRenderedText(env, "CPU 是 12%"))
}

func TestValidateRenderedTextAllowsKnownFacts(t *testing.T) {
	err := ValidateRenderedText(testResourceEnvelope(), "train-a 当前状态是 Running")
	assert.NoError(t, err)
}
