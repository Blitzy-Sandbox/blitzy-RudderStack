package alert

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEmail_Alert_Success verifies that the Email provider successfully
// delivers an alert message through a mock SMTP server. It validates the
// full SMTP protocol exchange (EHLO → MAIL FROM → RCPT TO → DATA → QUIT)
// and inspects the captured email payload for the correct subject line
// (containing instanceName) and body (containing the alert message).
func TestEmail_Alert_Success(t *testing.T) {
	Init()

	// Set up a TCP listener as a mock SMTP server with dynamic port allocation.
	// Using port 0 lets the OS assign an available ephemeral port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	// Buffered channel to receive the captured email data from the mock server goroutine.
	// Buffer size 1 prevents the goroutine from blocking if the test hasn't reached
	// the receive statement yet.
	capturedData := make(chan string, 1)

	// Start a goroutine that implements a minimal SMTP server.
	// It reads and responds to each protocol step, capturing the email
	// data payload sent between the DATA command and the dot-terminator.
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			capturedData <- ""
			return
		}
		defer conn.Close()

		// Set a deadline to prevent the test from hanging indefinitely
		// if the SMTP client never connects or stalls mid-exchange.
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

		reader := bufio.NewReader(conn)

		// Step 1: Send the SMTP greeting (220 service ready).
		fmt.Fprintf(conn, "220 mock SMTP ready\r\n")

		// Step 2: Read EHLO command from the client, respond with 250 OK.
		// Go's net/smtp client sends "EHLO localhost" after connecting.
		_, _ = reader.ReadString('\n')
		fmt.Fprintf(conn, "250 OK\r\n")

		// Step 3: Read MAIL FROM command, respond with 250 OK.
		_, _ = reader.ReadString('\n')
		fmt.Fprintf(conn, "250 OK\r\n")

		// Step 4: Read RCPT TO command, respond with 250 OK.
		_, _ = reader.ReadString('\n')
		fmt.Fprintf(conn, "250 OK\r\n")

		// Step 5: Read DATA command, respond with 354 to start mail input.
		_, _ = reader.ReadString('\n')
		fmt.Fprintf(conn, "354 Start mail input\r\n")

		// Step 6: Read email data line by line until the dot-terminator (".\r\n").
		// The SMTP protocol defines that a line consisting of only ".\r\n"
		// signals the end of the message data.
		var dataBuilder strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				break
			}
			if line == ".\r\n" {
				break
			}
			dataBuilder.WriteString(line)
		}

		// Step 7: Respond with 250 OK after successful data receipt.
		fmt.Fprintf(conn, "250 OK\r\n")

		// Step 8: Read QUIT command, respond with 221 Bye.
		_, _ = reader.ReadString('\n')
		fmt.Fprintf(conn, "221 Bye\r\n")

		// Send the captured email data back to the test goroutine.
		capturedData <- dataBuilder.String()
	}()

	// Extract host and port from the dynamically allocated listener address.
	tcpAddr := listener.Addr().(*net.TCPAddr)
	host := tcpAddr.IP.String()
	port := fmt.Sprintf("%d", tcpAddr.Port)

	// Create the Email struct with no authentication credentials.
	// When authUser and authPassword are empty, email.go skips smtp.PlainAuth,
	// which is appropriate for the mock server that doesn't advertise AUTH.
	email := &Email{
		smtpHost:     host,
		smtpPort:     port,
		from:         "alerts@rudderstack.com",
		to:           "ops@rudderstack.com",
		authUser:     "",
		authPassword: "",
		instanceName: "test-instance",
	}

	// Execute the Alert method — this drives the full SMTP exchange
	// with the mock server running in the goroutine above.
	email.Alert("Test pipeline alert")

	// Wait for the mock server goroutine to complete and verify the captured email data.
	select {
	case data := <-capturedData:
		// Verify the mock server received a non-empty email payload.
		require.True(t, len(data) > 0, "mock server should have received email data")

		// Verify the email contains instanceName in the subject line.
		// The email.go implementation constructs: Subject: [Alert] <instanceName>: <message>
		require.True(t, strings.Contains(data, "test-instance"),
			"email data should contain instanceName in subject")

		// Verify the email body contains the alert message text.
		require.Contains(t, data, "Test pipeline alert")

		// Verify the subject uses the [Alert] prefix format.
		require.Contains(t, data, "[Alert]")

		// Verify standard email headers are present.
		require.Contains(t, data, "From: alerts@rudderstack.com")
		require.Contains(t, data, "To: ops@rudderstack.com")
	case <-time.After(10 * time.Second):
		t.Fatal("mock SMTP server did not complete in time")
	}
}

// TestEmail_Alert_ConnectionFailure verifies that the Email provider handles
// SMTP connection failures gracefully. When smtp.SendMail cannot connect to
// the configured host:port, the Alert method should log the error via
// pkgLogger.Errorn and return without panicking. The Alert(string) method
// has no return value — errors are handled internally, matching the
// AlertManager interface contract used by PagerDuty and VictorOps.
func TestEmail_Alert_ConnectionFailure(t *testing.T) {
	Init()

	// Create an Email struct pointing to an unreachable port.
	// Port 19999 should not have anything listening on it in the test environment.
	email := &Email{
		smtpHost:     "127.0.0.1",
		smtpPort:     "19999",
		from:         "alerts@test.com",
		to:           "ops@test.com",
		authUser:     "",
		authPassword: "",
		instanceName: "test-instance",
	}

	// The Alert method should handle the TCP connection failure gracefully.
	// It logs the error via pkgLogger.Errorn (same pattern as pagerduty.go
	// line 39) and returns without panicking or propagating the error.
	require.NotPanics(t, func() {
		email.Alert("Connection failure test")
	})
}

// TestEmail_Alert_AuthenticationFailure verifies that the Email provider
// handles SMTP authentication rejection gracefully. A mock SMTP server
// advertises AUTH PLAIN support but responds with 535 to reject credentials.
// The Alert method should log the auth failure and return without panicking.
func TestEmail_Alert_AuthenticationFailure(t *testing.T) {
	Init()

	// Set up a TCP listener as a mock SMTP server that rejects authentication.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	// Channel to signal when the mock server goroutine finishes.
	done := make(chan struct{})

	// Start mock SMTP server that accepts the connection, advertises AUTH PLAIN
	// in the EHLO response, but rejects the AUTH command with 535.
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		// Set deadline to prevent test hangs.
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

		reader := bufio.NewReader(conn)

		// Step 1: Send SMTP greeting.
		fmt.Fprintf(conn, "220 mock SMTP ready\r\n")

		// Step 2: Read EHLO command and respond with a multi-line 250 response
		// that advertises AUTH PLAIN support. Go's net/smtp client parses
		// multi-line responses (250-xxx continuation, 250 xxx final line)
		// and extracts the AUTH extension with supported mechanisms.
		_, _ = reader.ReadString('\n')
		fmt.Fprintf(conn, "250-mock Hello\r\n250-AUTH PLAIN\r\n250 OK\r\n")

		// Step 3: Read AUTH PLAIN command (contains base64-encoded credentials)
		// and reject it with 535. Go's smtp.Client.Auth() interprets 535 as
		// an authentication failure error, closes the connection, and returns
		// the error to smtp.SendMail which propagates it to email.Alert.
		_, _ = reader.ReadString('\n')
		fmt.Fprintf(conn, "535 Authentication failed\r\n")

		// The client calls c.Close() after auth failure — no QUIT is expected.
		// The goroutine exits cleanly when the connection closes.
	}()

	// Extract host and port from the dynamically allocated listener address.
	tcpAddr := listener.Addr().(*net.TCPAddr)
	host := tcpAddr.IP.String()
	port := fmt.Sprintf("%d", tcpAddr.Port)

	// Create Email struct with authentication credentials that will be rejected.
	// Since smtpHost is 127.0.0.1 (localhost), Go's smtp.PlainAuth allows
	// the unencrypted connection (it has a localhost exception).
	email := &Email{
		smtpHost:     host,
		smtpPort:     port,
		from:         "alerts@test.com",
		to:           "ops@test.com",
		authUser:     "baduser",
		authPassword: "badpassword",
		instanceName: "test-instance",
	}

	// The Alert method should handle the authentication failure gracefully.
	// email.go catches the smtp.SendMail error, logs it via pkgLogger.Errorn,
	// and returns without panicking.
	require.NotPanics(t, func() {
		email.Alert("Auth failure test")
	})

	// Wait for the mock server goroutine to finish cleanly.
	select {
	case <-done:
		// Server completed as expected.
	case <-time.After(10 * time.Second):
		// Timeout is acceptable — the server may not exit cleanly
		// after the client disconnects following auth rejection.
	}
}
