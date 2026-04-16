package alert

import (
	"fmt"
	"net"
	"net/smtp"

	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// Email implements AlertManager for SMTP email notifications.
// It sends alert messages via the configured SMTP server using Go's
// net/smtp package. Authentication is optional — when authUser and
// authPassword are both non-empty, smtp.PlainAuth is used; otherwise
// the connection proceeds without authentication (useful for local
// relay or testing scenarios).
type Email struct {
	smtpHost     string
	smtpPort     string
	from         string
	to           string
	authUser     string
	authPassword string
	instanceName string
}

// Alert sends an email alert with the given message via SMTP.
// The email subject includes the instance name for quick identification,
// and the body contains the full alert message along with instance context.
// Errors during SMTP delivery are logged and do not propagate — this
// matches the AlertManager interface contract used by PagerDuty and VictorOps.
func (e *Email) Alert(message string) {
	subject := fmt.Sprintf("[Alert] %s: %s", e.instanceName, message)
	body := fmt.Sprintf(
		"Subject: %s\r\nFrom: %s\r\nTo: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=\"utf-8\"\r\n\r\n%s\r\n\r\nInstance: %s\r\n",
		subject,
		e.from,
		e.to,
		message,
		e.instanceName,
	)

	// Set up SMTP authentication only when credentials are provided.
	// A nil auth value causes smtp.SendMail to skip the AUTH step,
	// which is correct for unauthenticated local relay servers.
	var auth smtp.Auth
	if e.authUser != "" && e.authPassword != "" {
		auth = smtp.PlainAuth("", e.authUser, e.authPassword, e.smtpHost)
	}

	addr := net.JoinHostPort(e.smtpHost, e.smtpPort)
	err := smtp.SendMail(addr, auth, e.from, []string{e.to}, []byte(body))
	if err != nil {
		pkgLogger.Errorn("Alert: Failed to send email alert", obskit.Error(err))
		return
	}

	pkgLogger.Infon("Alert: Email sent successfully")
}
