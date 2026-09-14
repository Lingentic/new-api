package model

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/logger"
)

func TestAuthenticateIAMAPIKeyBoundaries(t *testing.T) {
	const rawKey = "fixture-model-key"
	for _, test := range []struct {
		name   string
		secret string
		ip     string
		change func(*testing.T)
		want   error
	}{
		{name: "valid with zero user balance", secret: "sk-" + rawKey, ip: "203.0.113.8"},
		{name: "IPv4 mapped IPv6", secret: "sk-" + rawKey, ip: "::ffff:203.0.113.8"},
		{name: "wrong key", secret: "sk-wrong", ip: "203.0.113.8", want: ErrIAMAPIKeyUnauthorized},
		{name: "wrong key case", secret: "sk-FIXTURE-model-key", ip: "203.0.113.8", want: ErrIAMAPIKeyUnauthorized},
		{name: "channel suffix is not truncated", secret: "sk-" + rawKey + "-1", ip: "203.0.113.8", want: ErrIAMAPIKeyUnauthorized},
		{name: "IP denied", secret: "sk-" + rawKey, ip: "198.51.100.1", want: ErrIAMAPIKeyAccessDenied},
		{name: "invalid IP", secret: "sk-" + rawKey, ip: "unknown", want: ErrIAMAPIKeyAccessDenied},
		{name: "local token", secret: "sk-" + rawKey, ip: "203.0.113.8", change: func(t *testing.T) {
			require.NoError(t, DB.Model(&Token{}).Where("id = ?", 2).Update("management_source", ManagementSourceLocal).Error)
		}, want: ErrIAMAPIKeyUnauthorized},
		{name: "expired", secret: "sk-" + rawKey, ip: "203.0.113.8", change: func(t *testing.T) {
			require.NoError(t, DB.Model(&Token{}).Where("id = ?", 2).Update("expired_time", time.Now().Add(-time.Minute).Unix()).Error)
		}, want: ErrIAMAPIKeyUnauthorized},
		{name: "disabled token", secret: "sk-" + rawKey, ip: "203.0.113.8", change: func(t *testing.T) {
			require.NoError(t, DB.Model(&Token{}).Where("id = ?", 2).Update("status", common.TokenStatusDisabled).Error)
		}, want: ErrIAMAPIKeyUnauthorized},
		{name: "revoked link", secret: "sk-" + rawKey, ip: "203.0.113.8", change: func(t *testing.T) {
			require.NoError(t, DB.Model(&IAMAPIKeyLink{}).Where("token_id = ?", 2).Update("desired_state", IAMAPIKeyStateRevoked).Error)
		}, want: ErrIAMAPIKeyUnauthorized},
		{name: "identity disabled", secret: "sk-" + rawKey, ip: "203.0.113.8", change: func(t *testing.T) {
			require.NoError(t, DB.Model(&IAMIdentityLink{}).Where("iam_user_id = ?", 11).Update("desired_state", IAMIdentityStateDisabled).Error)
		}, want: ErrIAMAPIKeyAccessDenied},
		{name: "user disabled", secret: "sk-" + rawKey, ip: "203.0.113.8", change: func(t *testing.T) {
			require.NoError(t, DB.Model(&User{}).Where("id = ?", 1).Update("status", common.UserStatusDisabled).Error)
		}, want: ErrIAMAPIKeyAccessDenied},
		{name: "cross tenant mapping", secret: "sk-" + rawKey, ip: "203.0.113.8", change: func(t *testing.T) {
			require.NoError(t, DB.Model(&IAMAPIKeyLink{}).Where("token_id = ?", 2).Update("new_api_user_id", 99).Error)
		}, want: ErrIAMAPIKeyUnauthorized},
		{name: "group override denied", secret: "sk-" + rawKey, ip: "203.0.113.8", change: func(t *testing.T) {
			require.NoError(t, DB.Model(&Token{}).Where("id = ?", 2).Update("group", "privileged").Error)
		}, want: ErrIAMAPIKeyAccessDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupIAMManagedTestDB(t)
			ips := "203.0.113.0/24"
			require.NoError(t, DB.Create(&User{Id: 1, Username: "fixture", Status: common.UserStatusEnabled, ManagementSource: ManagementSourceIAM, Quota: 0}).Error)
			require.NoError(t, DB.Create(&Token{Id: 2, UserId: 1, Key: rawKey, Status: common.TokenStatusEnabled, ManagementSource: ManagementSourceIAM, ExpiredTime: -1, UnlimitedQuota: true, ModelLimitsEnabled: true, ModelLimits: "beta,alpha", AllowIps: &ips}).Error)
			require.NoError(t, DB.Create(&IAMIdentityLink{IAMUserID: 11, NewAPIUserID: 1, OrganizationID: 21, OrganizationType: 2, DesiredState: IAMIdentityStateActive, LastVersion: 3}).Error)
			require.NoError(t, DB.Create(&IAMAPIKeyLink{IAMAPIKeyID: 31, IAMUserID: 11, NewAPIUserID: 1, TokenID: 2, DesiredState: IAMAPIKeyStateActive, LastVersion: 1}).Error)
			if test.change != nil {
				test.change(t)
			}
			var logs bytes.Buffer
			DB.Logger = logger.New(log.New(&logs, "", 0), logger.Config{LogLevel: logger.Info})
			principal, err := AuthenticateIAMAPIKey(context.Background(), test.secret, test.ip)
			require.NotContains(t, logs.String(), rawKey)
			if test.want != nil {
				require.ErrorIs(t, err, test.want)
				require.NotContains(t, err.Error(), rawKey)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 11, principal.IAMUserID)
			require.EqualValues(t, 21, principal.OrganizationID)
			require.EqualValues(t, 31, principal.APIKeyID)
			require.ElementsMatch(t, []string{"alpha", "beta"}, principal.ModelLimits)
			encoded, err := common.Marshal(principal)
			require.NoError(t, err)
			require.False(t, strings.Contains(string(encoded), rawKey))
			var token Token
			require.NoError(t, DB.First(&token, 2).Error)
			require.Zero(t, token.UsedQuota)
			require.Zero(t, token.AccessedTime)
		})
	}
}
