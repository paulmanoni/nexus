# Mail

`extension/mail` gives you one `Mailer` interface for outgoing mail. The transport is
chosen by config: log messages in development, send over SMTP in production.

```go
import "github.com/paulmanoni/nexus/extension/mail"

type Mailer struct{ *mail.Manager }

nexus.Boot(
    mail.BindFromConfig[Mailer]("smtp"),   // reads [mail.smtp]
)
```

```toml
[mail.smtp]
driver       = "smtp"
host         = "smtp.example.com"
port         = 587
username     = "${SMTP_USER}"
password     = "${SMTP_PASSWORD}"
encryption   = "starttls"      # none | starttls | tls
from_address = "no-reply@example.com"
from_name    = "Shop"
```

Send from any handler that takes `*Mailer`:

```go
err := m.Send(ctx, mail.Message{
    To:      []string{"alice@example.com"},
    Subject: "Your order has shipped",
    Text:    "Order #1042 is on its way.",
    HTML:    "<p>Order <b>#1042</b> is on its way.</p>",
})
```

- Text plus HTML becomes `multipart/alternative`.
- Attachments make the message `multipart/mixed`.
- Recipients are validated.
- `Cc`, `Bcc`, `ReplyTo` and `Headers` are supported.

Two backends, neither needing a third-party mail library:

- **`log`** is the default when `driver` is empty. It prints messages and sends nothing,
  which suits development and tests. `.Sent()` returns the messages for assertions.
- **`smtp`** uses the standard library's `net/smtp`. It supports STARTTLS on 587,
  implicit TLS on 465, and PLAIN auth.
