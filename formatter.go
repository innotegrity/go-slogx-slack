package slogxslack

import (
	"context"
	"encoding"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"go.innotegrity.dev/slogx"
	"go.innotegrity.dev/slogx/formatter"
)

const (
	// SlackMessageFormatterSourcePrefix is the default text to prepend when outputting the source location.
	SlackMessageFormatterSourcePrefix = "Source:\t\t\t"

	// SlackMessageFormatterTimeAttr is the default text to prepend when outputting the time of the record.
	SlackMessageFormatterTimePrefix = "Occurred at:\t"
)

// SlackMessageFormatter describes the interface a formatter which outputs a record to a Slack message must implement.
type SlackMessageFormatter interface {
	// FormatRecord should take the data from the record and format it as needed, storing it in the returned
	// webhook message.
	FormatRecord(context.Context, time.Time, slogx.Level, uintptr, string, []slog.Attr) (*slack.WebhookMessage, error)
}

// slackMessageFormatterOptionsContext can be used to retrieve the options used by the formatter from the context.
type slackMessageFormatterOptionsContext struct{}

// SlackMessageFormatterOptions holds the options for the message formatter.
type SlackMessageFormatterOptions struct {
	// ApplicationIconURL is a URL to an icon to display next to the application name in the output message.
	//
	// If this is empty, no icon is shown next to the application name.
	ApplicationIconURL string

	// ApplicationName is the name of the application to display above the message.
	//
	// If this is empty, no application name is shown.
	ApplicationName string

	// AttrFormatter is the middleware formatting function to call to format any attribute.
	//
	// Attribute values should be resolved by the handler before formatting. Any value returned by the formatter should
	// be resolved prior to return.
	//
	// If nil, attributes are simply printed unchanged.
	AttrFormatter formatter.FormatAttrFn

	// IgnoreAttrs is a list of regular expressions to use for matching attributes which should not be printed.
	//
	// Note that this only applies to attributes and not defined parts like the level, message, source or time.
	//
	// If any regular expression does not compile, it is simply ignored.
	IgnoreAttrs []string

	// IncludeAttrs indicates whether or not to include attributes in the Slack message.
	IncludeAttrs bool

	// IncludeSource indicates whether or not to include source file location information in the Slack mesage.
	IncludeSource bool

	// LevelFormatter is the middleware formatting function to call to format the level.
	//
	// If nil, the level is printed using FormatLevelValueDefault().
	LevelFormatter formatter.FormatLevelValueFn

	// MessageFormatter is the middlware formatting function to call to format the message.
	//
	// If nil, the message is printed as-is.
	MessageFormatter formatter.FormatMessageValueFn

	// SortAttrs indicates whether or not to sort the attributes alphabetically before adding them to the message.
	SortAttrs bool

	// SourcePrefix is the text to prefix the source information with in the output message.
	//
	// If this is empty, the default value of "Source:\t\t\t" is used.
	SourcePrefix string

	// SourceFormatter is the middleware formatting function to call to format the source code location where the record
	// was created.
	//
	// If nil, the source code location is printed using FormatSourceValueDefault().
	SourceFormatter formatter.FormatSourceValueFn

	// SpecificAttrFormatter is the middleware formatting function to call to format a specific attribute.
	//
	// The key for the map corresponds to the name of the specific attribute to format. If an attribute is nested within
	// a group, use a single period (.) to designate the group and attribute (eg: GROUP.ATTRIBUTE). Nested groups use
	// the same format (eg: GROUP1.GROUP2.ATTRIBUTE).
	//
	// Attribute values should be resolved by the handler before formatting. Any value returned by the formatter should
	// be resolved prior to return.
	//
	// If nil or if the attribute does not exist in the map, the default is to fall back to the AttrFormatter function.
	SpecificAttrFormatter map[string]formatter.FormatAttrFn

	// TimePrefix is the text to prefix the record timestamp with in the output message.
	//
	// If this is empty, the default value of "Occurred at:\t" is used.
	TimePrefix string

	// TimeFormatter is the middleware formatting function to call to the time of the record.
	//
	// If nil, the time is printed using FormatTimeValueDefault().
	TimeFormatter formatter.FormatTimeValueFn
}

// DefaultSlackMessageFormatterOptions returns a default set of options for the Slack message formatter.
func DefaultSlackMessageFormatterOptions() SlackMessageFormatterOptions {
	return SlackMessageFormatterOptions{
		IgnoreAttrs:           []string{},
		IncludeAttrs:          true,
		LevelFormatter:        formatSlackMessageLevelDefault,
		SortAttrs:             true,
		SourcePrefix:          SlackMessageFormatterSourcePrefix,
		SourceFormatter:       formatter.FormatSourceValueDefault,
		SpecificAttrFormatter: map[string]formatter.FormatAttrFn{},
		TimePrefix:            SlackMessageFormatterTimePrefix,
		TimeFormatter: func(ctx context.Context, level slog.Leveler, t time.Time) (string, error) {
			return t.Local().Format("03:04:05PM MST"), nil
		},
	}
}

// GetSlackMessageFormatterOptionsFromContext retrieves the options from the context.
//
// If the options are not set in the context, a set of default options is returned instead.
func GetSlackMessageFormatterOptionsFromContext(ctx context.Context) *SlackMessageFormatterOptions {
	o := ctx.Value(slackMessageFormatterOptionsContext{})
	if o != nil {
		if opts, ok := o.(*SlackMessageFormatterOptions); ok {
			return opts
		}
	}
	opts := DefaultSlackMessageFormatterOptions()
	return &opts
}

// AddToContext adds the options to the given context and returns the new context.
func (o *SlackMessageFormatterOptions) AddToContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, slackMessageFormatterOptionsContext{}, o)
}

// slackMessageFormatter formats records for output as Slack messages.
type slackMessageFormatter struct {
	// unexported variables
	ignoredAttrPatterns []*regexp.Regexp
	options             SlackMessageFormatterOptions
}

// DefaultSlackMessageFormatter returns a Slack message formatter with typical defaults already set.
func DefaultSlackMessageFormatter() *slackMessageFormatter {
	return NewSlackMessageFormatter(DefaultSlackMessageFormatterOptions())
}

// NewSlackMessageFormatter creates and returns a new slack message formatter.
func NewSlackMessageFormatter(opts SlackMessageFormatterOptions) *slackMessageFormatter {
	// set default options
	if opts.TimePrefix == "" {
		opts.TimePrefix = SlackMessageFormatterTimePrefix
	}
	if opts.IncludeSource && opts.SourcePrefix == "" {
		opts.SourcePrefix = SlackMessageFormatterSourcePrefix
	}

	// create the formatter object
	f := &slackMessageFormatter{
		ignoredAttrPatterns: []*regexp.Regexp{},
		options:             opts,
	}
	for _, p := range opts.IgnoreAttrs {
		regex, err := regexp.Compile(p)
		if err == nil {
			f.ignoredAttrPatterns = append(f.ignoredAttrPatterns, regex)
		}
	}
	return f
}

// FormatRecord handles formatting the given record and outputting it into the returned Slack message for consumption
// by a handler.
//
// By default, duration values in attributes are formatted using the String() function and time values are formatted
// in UTC time using the RFC3339 layout.
func (f *slackMessageFormatter) FormatRecord(ctx context.Context, timestamp time.Time, level slogx.Level, pc uintptr,
	msg string, attrs []slog.Attr) (*slack.WebhookMessage, error) {

	var err error
	var strVal string
	handlerCtx := f.options.AddToContext(ctx)

	// initialize the message
	message := &slack.WebhookMessage{
		Blocks: &slack.Blocks{
			BlockSet: []slack.Block{
				slack.DividerBlock{
					Type: slack.MBTDivider,
				},
			},
		},
	}

	// add the application name and level context
	appLevelContextElements := []slack.MixedElement{}
	if f.options.ApplicationIconURL != "" {
		appLevelContextElements = append(appLevelContextElements, slack.ImageBlockElement{
			Type:     slack.METImage,
			ImageURL: f.options.ApplicationIconURL,
			AltText:  f.options.ApplicationName,
		})
	}
	if f.options.ApplicationName != "" {
		appLevelContextElements = append(appLevelContextElements, slack.TextBlockObject{
			Type: slack.MarkdownType,
			Text: f.options.ApplicationName,
		})
	}
	if f.options.LevelFormatter != nil {
		strVal, err = f.options.LevelFormatter(handlerCtx, level)
	} else {
		strVal, err = formatter.FormatLevelValueDefault(handlerCtx, level)
	}
	if err != nil {
		return nil, err
	}
	appLevelContextElements = append(appLevelContextElements, slack.TextBlockObject{
		Type: slack.MarkdownType,
		Text: strVal,
	})
	message.Blocks.BlockSet = append(message.Blocks.BlockSet, slack.NewContextBlock("", appLevelContextElements...))

	// add the time and source (if requested)
	timeSourceText := f.options.TimePrefix
	if f.options.TimeFormatter != nil {
		strVal, err = f.options.TimeFormatter(handlerCtx, level, timestamp)
	} else {
		strVal, err = formatter.FormatTimeValueDefault(handlerCtx, level, timestamp)
	}
	if err != nil {
		return nil, err
	}
	timeSourceText += strVal
	if f.options.IncludeSource {
		timeSourceText = fmt.Sprintf("%s\n%s", timeSourceText, f.options.SourcePrefix)
		if f.options.SourceFormatter != nil {
			strVal, err = f.options.SourceFormatter(handlerCtx, level, pc)
		} else {
			strVal, err = formatter.FormatSourceValueDefault(handlerCtx, level, pc)
		}
		if err != nil {
			return nil, err
		}
		timeSourceText += strVal
	}
	message.Blocks.BlockSet = append(message.Blocks.BlockSet, slack.NewContextBlock("", slack.TextBlockObject{
		Type: slack.MarkdownType,
		Text: timeSourceText,
	}))

	// add the message
	message.Blocks.BlockSet = append(message.Blocks.BlockSet,
		slack.DividerBlock{
			Type: slack.MBTDivider,
		},
		slack.SectionBlock{
			Type: slack.MBTSection,
			Text: &slack.TextBlockObject{
				Type: slack.MarkdownType,
				Text: msg,
			},
		},
	)

	// add attributes (if requested)
	if f.options.IncludeAttrs {
		if f.options.SortAttrs {
			attrs = slogx.SortAttrs(attrs)
		}
		flattenedAttrs := slogx.FlattenAttrs(attrs)
		for _, attr := range flattenedAttrs {
			element, err := f.attrToElement(handlerCtx, level, attr.Key, attr.Value)
			if err != nil {
				return nil, err
			}
			if element != nil {
				message.Blocks.BlockSet = append(message.Blocks.BlockSet, slack.NewContextBlock("", element))
			}
		}
	}
	return message, nil
}

// attrToElement converts the given attribute into a Slack context element.
func (f slackMessageFormatter) attrToElement(ctx context.Context, level slog.Leveler, attrKey string,
	attrValue slog.Value) (slack.MixedElement, error) {

	// ignore the attribute if the key matches
	for _, p := range f.ignoredAttrPatterns {
		if p.MatchString(attrKey) {
			return nil, nil
		}
	}

	// extract the group name and attribute from the key
	group := ""
	actualAttrKey := attrKey
	groupIndex := strings.LastIndex(attrKey, ".")
	if groupIndex != -1 {
		group = attrKey[:groupIndex]
		actualAttrKey = attrKey[groupIndex+1:]
	}

	// format the attribute using any formatter functions first
	formattedKey := attrKey
	formattedValue := attrValue.Resolve()
	var err error
	if fn, ok := f.options.SpecificAttrFormatter[attrKey]; ok && fn != nil {
		formattedKey, formattedValue, err = fn(ctx, level, group, actualAttrKey, formattedValue)
		if err != nil {
			return nil, err
		}
	} else if f.options.AttrFormatter != nil {
		formattedKey, formattedValue, err = f.options.AttrFormatter(ctx, level, group, actualAttrKey, formattedValue)
		if err != nil {
			return nil, err
		}
	}

	// format the key/value
	element := slack.TextBlockObject{
		Type: slack.MarkdownType,
	}
	switch formattedValue.Kind() {
	case slog.KindBool:
		element.Text = fmt.Sprintf("*%s*: `%t`", formattedKey, formattedValue.Bool())
	case slog.KindString:
		element.Text = fmt.Sprintf("*%s*: `%s`", formattedKey, formattedValue.String())
	case slog.KindDuration:
		element.Text = fmt.Sprintf("*%s*: `%s`", formattedKey, formattedValue.Duration().String())
	case slog.KindTime:
		element.Text = fmt.Sprintf("*%s*: `%s`", formattedKey, formattedValue.Time().UTC().Format(time.RFC3339))
	case slog.KindFloat64:
		element.Text = fmt.Sprintf("*%s*: `%f`", formattedKey, formattedValue.Float64())
	case slog.KindInt64:
		element.Text = fmt.Sprintf("*%s*: `%d`", formattedKey, formattedValue.Int64())
	case slog.KindUint64:
		element.Text = fmt.Sprintf("*%s*: `%d`", formattedKey, formattedValue.Uint64())
	case slog.KindGroup: // should never occur as the attrs have been flattened
		element.Text = fmt.Sprintf("*%s*: `%+v`", formattedKey, formattedValue.Group())
	default:
		if tm, ok := formattedValue.Any().(encoding.TextMarshaler); ok {
			output, err := tm.MarshalText()
			if err != nil {
				return nil, err
			}
			element.Text = fmt.Sprintf("*%s*: `%s`", formattedKey, string(output))
		} else {
			element.Text = fmt.Sprintf("*%s*: `%+v`", formattedKey, formattedValue.Any())
		}
	}
	return element, nil
}

// formatSlackMessageLevelDeafult formats the level using an emoji prefix.
func formatSlackMessageLevelDefault(ctx context.Context, level slog.Leveler) (string, error) {
	switch level {
	case slogx.LevelTrace:
		return ":eyes: trace", nil
	case slogx.LevelDebug:
		return ":ladybug: debug", nil
	case slogx.LevelInfo:
		return ":information_source: info", nil
	case slogx.LevelNotice:
		return ":grey_exclamation: notice", nil
	case slogx.LevelWarn:
		return ":warning: warn", nil
	case slogx.LevelError:
		return ":no_entry: error", nil
	case slogx.LevelFatal:
		return ":rotating_light: fatal", nil
	case slogx.LevelPanic:
		return ":sos: panic", nil
	}
	return fmt.Sprintf("%s", level), nil
}
