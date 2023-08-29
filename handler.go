package slogxslack

import (
	"context"
	"errors"
	"net/http"

	"github.com/slack-go/slack"
	"go.innotegrity.dev/async"
	"go.innotegrity.dev/generic"
	"go.innotegrity.dev/slogx"
	"golang.org/x/exp/slog"
)

// SlackHandlerOptionsContext can be used to retrieve the options used by the handler from the context.
type SlackHandlerOptionsContext struct{}

// SlackHandlerOptions holds the options for the Slack handler.
type SlackHandlerOptions struct {
	// EnableAsync will execute the Handle() function in a separate goroutine.
	//
	// When async is enabled, you should be sure to call the Shutdown() function or use the slogx.Shutdown()
	// function to ensure all goroutines are finished and any pending records have been written.
	EnableAsync bool

	// HTTPClient allows for the use of a custom HTTP client for posting the webhook message.
	//
	// If nil, http.DefaultClient is used.
	HTTPClient *http.Client

	// Level is the minimum log level to write to the handler.
	//
	// By default, the level will be set to slog.LevelInfo if not supplied.
	Level slog.Leveler

	// RecordFormatter specifies the formatter to use to format the record before sending it to Slack.
	//
	// If no formatter is supplied, formatters.DefaultSlackMessageFormatter is used to format the output.
	RecordFormatter SlackMessageFormatter

	// WebhookURL is the Slack webhook URL to use in order to send the message.
	//
	// This is a required option.
	WebhookURL string
}

// slackHandler is a log handler that writes records to Slack via a webhook.
type slackHandler struct {
	activeGroup string
	attrs       []slog.Attr
	futures     []async.Future
	groups      []string
	options     SlackHandlerOptions
}

// NewSlackHandler creates a new handler object.
func NewSlackHandler(opts SlackHandlerOptions) (*slackHandler, error) {
	// validate required options
	if opts.WebhookURL == "" {
		return nil, errors.New("webhook URL is required and cannot be empty")
	}

	// set default options
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.Level == nil {
		opts.Level = slog.LevelInfo
	}

	// create the handler
	return &slackHandler{
		attrs:   []slog.Attr{},
		futures: []async.Future{},
		groups:  []string{},
		options: opts,
	}, nil
}

// Enabled determines whether or not the given level is enabled in this handler.
func (h slackHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.options.Level.Level()
}

// Handle actually handles posting the record to the Slack webhook.
//
// Any attributes duplicated between the handler and record, including within groups, are automaticlaly removed.
// If a duplicate is encountered, the last value found will be used for the attribute's value.
func (h *slackHandler) Handle(ctx context.Context, r slog.Record) error {
	handlerCtx := context.WithValue(ctx, SlackHandlerOptionsContext{}, &h.options)
	if !h.options.EnableAsync {
		return h.handle(handlerCtx, r)
	}

	future := async.Exec(func() any {
		return h.handle(handlerCtx, r)
	})
	h.futures = append(h.futures, future)
	return nil
}

// Shutdown is responsible for cleaning up resources used by the handler.
func (h slackHandler) Shutdown(continueOnError bool) error {
	for _, f := range h.futures {
		if f != nil {
			f.Await()
		}
	}
	return nil
}

// WithAttrs creates a new handler from the existing one adding the given attributes to it.
func (h slackHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandler := &slackHandler{
		attrs:   h.attrs,
		futures: h.futures,
		groups:  h.groups,
		options: h.options,
	}
	if h.activeGroup == "" {
		newHandler.attrs = append(newHandler.attrs, attrs...)
	} else {
		newHandler.attrs = append(newHandler.attrs, slog.Group(h.activeGroup, generic.AnySlice(attrs)...))
		newHandler.activeGroup = h.activeGroup
	}
	return newHandler
}

// WithGroup creates a new handler from the existing one adding the given group to it.
func (h slackHandler) WithGroup(name string) slog.Handler {
	newHandler := &slackHandler{
		attrs:   h.attrs,
		futures: h.futures,
		groups:  h.groups,
		options: h.options,
	}
	if name != "" {
		newHandler.groups = append(newHandler.groups, name)
		newHandler.activeGroup = name
	}
	return newHandler
}

// handle is responsible for actually posting the message using the Slack webhook.
func (h slackHandler) handle(ctx context.Context, r slog.Record) error {
	attrs := slogx.ConsolidateAttrs(h.attrs, h.activeGroup, r)

	// format the output into a Slack message
	var message *slack.WebhookMessage
	var err error
	if h.options.RecordFormatter != nil {
		message, err = h.options.RecordFormatter.FormatRecord(ctx, r.Time, slogx.Level(r.Level), r.PC, r.Message,
			attrs)
	} else {
		f := DefaultSlackMessageFormatter()
		message, err = f.FormatRecord(ctx, r.Time, slogx.Level(r.Level), r.PC, r.Message, attrs)
	}
	if err != nil {
		return err
	}

	// send the message to Slack
	return slack.PostWebhookCustomHTTP(h.options.WebhookURL, h.options.HTTPClient, message)
}
