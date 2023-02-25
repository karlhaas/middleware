package i18n

import (
	"fmt"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v2"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gobuffalo/buffalo"
	"github.com/nicksnyder/go-i18n/v2/i18n"
)

// LanguageExtractor can be implemented for custom finding of search
// languages. This can be useful if you want to load a user's language
// from something like a database. See Middleware() for more information
// on how the default implementation searches for languages.
type LanguageExtractor func(LanguageExtractorOptions, buffalo.Context) []string

// LanguageExtractorOptions is a map of options for a LanguageExtractor.
type LanguageExtractorOptions map[string]interface{}

// Translator for handling all your i18n needs.
type Translator struct {
	// FS that contains the files
	FS fs.FS
	// DefaultLanguage - default is passed as a parameter on New.
	DefaultLanguage string
	// HelperName - name of the view helper. default is "t"
	HelperName string
	// LanguageExtractors - a sorted list of user language extractors.
	LanguageExtractors []LanguageExtractor
	// LanguageExtractorOptions - a map with options to give to LanguageExtractors.
	LanguageExtractorOptions LanguageExtractorOptions
	// Bundle is the i18n.Bundle instance
	Bundle *i18n.Bundle
	// The time the message files have been loaded
	loadingTime time.Time
}

// Load translations from the t.FS
func (t *Translator) Load() error {
	err := fs.WalkDir(t.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		b, err := fs.ReadFile(t.FS, path)
		if err != nil {
			return fmt.Errorf("unable to read locale file %s: %v", path, err)
		}

		base := filepath.Base(path)
		dir := filepath.Dir(path)

		// Add a prefix to the loaded string, to avoid collision with an ISO lang code
		_, err = t.Bundle.ParseMessageFileBytes(b, fmt.Sprintf("%sbuff%s", dir, base))
		if err != nil {
			return fmt.Errorf("unable to parse locale file %s: %v", base, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	t.loadingTime = time.Now().UTC()
	return nil
}

// AddTranslation directly, without using a file. This is useful if you wish to load translations
// from a database, instead of disk.
func (t *Translator) AddTranslation(lang language.Tag, messages ...*i18n.Message) error {
	return t.Bundle.AddMessages(lang, messages...)
}

// New Translator. Requires a fs.FS that points to the location
// of the translation files, as well as a default language. This will
// also call t.Load() and load the translations from disk.
func New(fsys fs.FS, defaultLanguage string) (*Translator, error) {
	defaultLanguageTag, err := language.Parse(defaultLanguage)
	if err != nil {
		return nil, fmt.Errorf("unable to parse default language %s: %v", defaultLanguage, err)
	}
	bundle := i18n.NewBundle(defaultLanguageTag)
	bundle.RegisterUnmarshalFunc("yaml", yaml.Unmarshal)

	t := &Translator{
		FS:              fsys,
		DefaultLanguage: defaultLanguage,
		HelperName:      "t",
		LanguageExtractorOptions: LanguageExtractorOptions{
			"CookieName":    "lang",
			"SessionName":   "lang",
			"URLPrefixName": "lang",
		},
		LanguageExtractors: []LanguageExtractor{
			CookieLanguageExtractor,
			SessionLanguageExtractor,
			HeaderLanguageExtractor,
		},
		Bundle: bundle,
	}
	return t, t.Load()
}

// Middleware for loading the translations for the language(s)
// selected. By default languages are loaded in the following order:
//
// Cookie - "lang"
// Session - "lang"
// Header - "Accept-Language"
// Default - "en-US"
//
// These values can be changed on the Translator itself. In development
// model the translation files will be reloaded on each request.
func (t *Translator) Middleware() buffalo.MiddlewareFunc {
	return func(next buffalo.Handler) buffalo.Handler {
		return func(c buffalo.Context) error {
			// in development reload the translations if a file has changed
			if t.needsReload(c) {
				err := t.Load()
				if err != nil {
					return err
				}
			}

			// set languages in context, if not set yet
			if langs := c.Value("languages"); langs == nil {
				c.Set("languages", t.extractLanguage(c))
			}

			// set translator
			if T := c.Value("T"); T == nil {
				langs := c.Value("languages").([]string)
				localizer := i18n.NewLocalizer(t.Bundle, langs...)
				c.Set("T", localizer)
			}

			// set up the helper function for the views:
			c.Set(t.HelperName, func(s string, i ...interface{}) (string, error) {
				return t.Translate(c, s, i...)
			})
			return next(c)
		}
	}
}

func (t *Translator) translate(localizer *i18n.Localizer, translationID string, args []interface{}) (string, error) {
	var pluralCount interface{}
	var templateData interface{}
	if len(args) > 0 {
		switch value := args[0].(type) {
		case int, int8, int16, int32, int64, float32, float64:
			pluralCount = value
			if len(args) > 1 {
				templateData = args[1]
			}
		default:
			templateData = args[0]
		}
	}
	config := i18n.LocalizeConfig{
		MessageID:    translationID,
		TemplateData: templateData,
		PluralCount:  pluralCount,
	}
	return localizer.Localize(&config)
}

func (t *Translator) needsReload(c buffalo.Context) bool {
	if c.Value("env").(string) != "development" {
		return false
	}
	nilTime := time.Time{}
	if nilTime == t.loadingTime {
		return true
	}
	result := false
	err := fs.WalkDir(t.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(t.loadingTime) {
			c.Logger().Infof("i18n middleware: Reloading translations because %s has changed", d.Name())
			result = true
		}
		return nil
	})
	if err != nil {
		c.Logger().Errorf("i18n middleware: Error in needsReload: %s", err)
	}
	return result
}

// Translate returns the translation of the string identified by translationID.
//
// See https://github.com/gobuffalo/i18n-mw/internal/go-i18n
//
// If there is no translation for translationID, then the translationID itself is returned.
// This makes it easy to identify missing translations in your app.
//
// If translationID is a non-plural form, then the first variadic argument may be a map[string]interface{}
// or struct that contains template data.
//
// If translationID is a plural form, the function accepts two parameter signatures
// 1. T(count int, data struct{})
// The first variadic argument must be an integer type
// (int, int8, int16, int32, int64) or a float formatted as a string (e.g. "123.45").
// The second variadic argument may be a map[string]interface{} or struct{} that contains template data.
// 2. T(data struct{})
// data must be a struct{} or map[string]interface{} that contains a Count field and the template data,
// Count field must be an integer type (int, int8, int16, int32, int64)
// or a float formatted as a string (e.g. "123.45").
func (t *Translator) Translate(c buffalo.Context, translationID string, args ...interface{}) (string, error) {
	return t.translate(c.Value("T").(*i18n.Localizer), translationID, args)
}

// TranslateWithLang returns the translation of the string identified by translationID, for the given language.
// See Translate for further details.
func (t *Translator) TranslateWithLang(lang, translationID string, args ...interface{}) (string, error) {
	return t.translate(i18n.NewLocalizer(t.Bundle, lang), translationID, args)
}

// AvailableLanguages gets the list of languages provided by the app.
func (t *Translator) AvailableLanguages() []string {
	tags := t.Bundle.LanguageTags()
	languages := make([]string, len(tags))
	for i, tag := range tags {
		languages[i] = tag.String()
	}
	sort.Strings(languages)
	return languages
}

// Refresh updates the context, reloading translation functions.
// It can be used after language change, to be able to use translation functions
// in the new language (for a flash message, for instance).
func (t *Translator) Refresh(c buffalo.Context, newLang string) {
	langs := []string{newLang}
	langs = append(langs, t.extractLanguage(c)...)

	// Refresh languages
	c.Set("languages", langs)

	localizer := i18n.NewLocalizer(t.Bundle, langs...)

	// Refresh translation engine
	c.Set("T", localizer)
}

func (t *Translator) extractLanguage(c buffalo.Context) []string {
	var langs []string
	for _, extractor := range t.LanguageExtractors {
		langs = append(langs, extractor(t.LanguageExtractorOptions, c)...)
	}
	// Add default language, even if no language extractor is defined
	langs = append(langs, t.DefaultLanguage)
	return langs
}

// CookieLanguageExtractor is a LanguageExtractor implementation, using a cookie.
func CookieLanguageExtractor(o LanguageExtractorOptions, c buffalo.Context) []string {
	langs := make([]string, 0)
	// try to get the language from a cookie:
	if cookieName := o["CookieName"].(string); cookieName != "" {
		if cookie, err := c.Request().Cookie(cookieName); err == nil {
			if cookie.Value != "" {
				langs = append(langs, cookie.Value)
			}
		}
	} else {
		c.Logger().Error("i18n middleware: \"CookieName\" is not defined in LanguageExtractorOptions")
	}
	return langs
}

// SessionLanguageExtractor is a LanguageExtractor implementation, using a session.
func SessionLanguageExtractor(o LanguageExtractorOptions, c buffalo.Context) []string {
	langs := make([]string, 0)
	// try to get the language from the session
	if sessionName := o["SessionName"].(string); sessionName != "" {
		if s := c.Session().Get(sessionName); s != nil {
			langs = append(langs, s.(string))
		}
	} else {
		c.Logger().Error("i18n middleware: \"SessionName\" is not defined in LanguageExtractorOptions")
	}
	return langs
}

// HeaderLanguageExtractor is a LanguageExtractor implementation, using a HTTP Accept-Language
// header.
func HeaderLanguageExtractor(o LanguageExtractorOptions, c buffalo.Context) []string {
	langs := make([]string, 0)
	// try to get the language from a header:
	acceptLang := c.Request().Header.Get("Accept-Language")
	if acceptLang != "" {
		langs = append(langs, parseAcceptLanguage(acceptLang)...)
	}
	return langs
}

// URLPrefixLanguageExtractor is a LanguageExtractor implementation, using a prefix in the URL.
func URLPrefixLanguageExtractor(o LanguageExtractorOptions, c buffalo.Context) []string {
	langs := make([]string, 0)
	// try to get the language from an URL prefix:
	if urlPrefixName := o["URLPrefixName"].(string); urlPrefixName != "" {
		paramLang := c.Param(urlPrefixName)
		if paramLang != "" && strings.HasPrefix(c.Request().URL.Path, fmt.Sprintf("/%s", paramLang)) {
			langs = append(langs, paramLang)
		}
	} else {
		c.Logger().Error("i18n middleware: \"URLPrefixName\" is not defined in LanguageExtractorOptions")
	}
	return langs
}

// Inspired from https://siongui.github.io/2015/02/22/go-parse-accept-language/
// Parse an Accept-Language string to get usable lang values for i18n system
func parseAcceptLanguage(acptLang string) []string {
	var lqs []string

	langQStrs := strings.Split(acptLang, ",")
	for _, langQStr := range langQStrs {
		trimedLangQStr := strings.Trim(langQStr, " ")

		langQ := strings.Split(trimedLangQStr, ";")
		lq := langQ[0]
		lqs = append(lqs, lq)
	}
	return lqs
}
