package alert

import (
	"errors"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
)

var (
	alertProvider       string
	pagerDutyRoutingKey string
	instanceName        string
	victorOpsRoutingKey string
	// Slack provider config
	slackWebhookURL string
	// Email provider config
	emailSMTPHost     string
	emailSMTPPort     string
	emailFrom         string
	emailTo           string
	emailAuthUser     string
	emailAuthPassword string
)

func Init() {
	loadConfig()
	pkgLogger = logger.NewLogger().Child("alert")
}

func loadConfig() {
	alertProvider = config.GetString("ALERT_PROVIDER", "victorops")
	pagerDutyRoutingKey = config.GetString("PG_ROUTING_KEY", "")
	instanceName = config.GetString("INSTANCE_ID", "")
	victorOpsRoutingKey = config.GetString("VICTOROPS_ROUTING_KEY", "")
	// Slack provider config
	slackWebhookURL = config.GetString("SLACK_WEBHOOK_URL", "")
	// Email provider config
	emailSMTPHost = config.GetString("EMAIL_SMTP_HOST", "")
	emailSMTPPort = config.GetString("EMAIL_SMTP_PORT", "587")
	emailFrom = config.GetString("EMAIL_FROM", "")
	emailTo = config.GetString("EMAIL_TO", "")
	emailAuthUser = config.GetString("EMAIL_AUTH_USER", "")
	emailAuthPassword = config.GetString("EMAIL_AUTH_PASSWORD", "")
}

// AlertManager interface
type AlertManager interface {
	Alert(string)
}

// New returns FileManager backed by configured privider
func New() (AlertManager, error) {
	switch alertProvider {
	case "victorops":
		return &VictorOps{
			routingKey:   victorOpsRoutingKey,
			instanceName: instanceName,
		}, nil
	case "pagerduty":
		return &PagerDuty{
			routingKey:   pagerDutyRoutingKey,
			instanceName: instanceName,
		}, nil
	case "slack":
		return &Slack{
			webhookURL:   slackWebhookURL,
			instanceName: instanceName,
		}, nil
	case "email":
		return &Email{
			smtpHost:     emailSMTPHost,
			smtpPort:     emailSMTPPort,
			from:         emailFrom,
			to:           emailTo,
			authUser:     emailAuthUser,
			authPassword: emailAuthPassword,
			instanceName: instanceName,
		}, nil
	}
	return nil, errors.New("no provider configured for Alert Manager")
}
