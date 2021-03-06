// Copyright 2018 Informo core team <core@informo.network>
//
// Licensed under the GNU Affero General Public License, Version 3.0
// (the "License"); you may not use this file except in compliance with the
// License.
// You may obtain a copy of the License at
//
//     https://www.gnu.org/licenses/agpl-3.0.html
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package crawler

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"common/config"
	"common/database"

	"github.com/PuerkitoBio/gocrawl"
	"github.com/PuerkitoBio/goquery"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/html"
)

// Extender implements gocrawl.Extender.
// Other fields also include the database, a logrus logger (with the "website"
// field prefilled), and channels for reporting errors to the parent goroutine
// or abort the process.
type Extender struct {
	gocrawl.DefaultExtender
	db              *database.Database
	website         *config.Website
	log             *logrus.Entry
	visitedArticles map[string]bool
	errChan         chan error
	abortChan       chan string
}

// NewExtender instantiate an Extender.
// Returns an error if an issue happened while loading the visited article's URLs
// from the database.
func NewExtender(
	db *database.Database, website *config.Website, log *logrus.Entry,
	errCh chan error, abortCh chan string,
) (*Extender, error) {
	// Load the URLs of visited articles from the database so we can use it to
	// filter the enqueuing process and speed the crawls up.
	visited, err := db.RetrieveArticleURLsForWebsite(website.Identifier)
	if err != nil {
		return nil, err
	}

	log.Infof("Loaded %d visited URLs for this website", len(visited))

	// Instantiate the extender.
	return &Extender{
		DefaultExtender: gocrawl.DefaultExtender{},
		db:              db,
		website:         website,
		log:             log,
		visitedArticles: visited,
		errChan:         errCh,
		abortChan:       abortCh,
	}, nil
}

// Filter implements gocrawl.Extender.Filter
// Tells the crawler if an URL should be enqueued for visiting, according to
// whether it has already been visited in the current crawl, or whether it matches
// the URL of an article that has already been saved in the database.
func (e *Extender) Filter(ctx *gocrawl.URLContext, isVisited bool) bool {
	// Remove the fragment (#foobar) part of the URL.
	// Because the context is a reference here, and because the same reference
	// is passed along all functions, removing the fragment here will ensure it
	// will be removed at every other point in the work flow (i.e. we won't save
	// an article's URL with a fragment part in the database).
	ctx.URL().Fragment = ""

	if e.website.Query != nil {
		// If required by the configuration, iterate over the keys from the query
		// (?foo=bar) part of the URL to only keep the ones set as exceptions,
		// or remove them, accordingly with the IgnoreAll value.
		if len(e.website.Query.Except) > 0 {
			q := ctx.URL().Query()
			// Iterate over the query keys.
			for k := range q {
				// If IgnoreAll is set to true, the default behaviour is to delete
				// every key that doesn't match with an exception. If it is set to
				// false, the default behaviour is to keep all keys except exceptions.
				var del = e.website.Query.IgnoreAll
				// Iterate over the exceptions.
				for _, exception := range e.website.Query.Except {
					if k == exception {
						del = !e.website.Query.IgnoreAll
					}
				}

				// If the key doesn't match any of the exceptions, delete it
				// along with its value.
				if del {
					q.Del(k)
				}
			}

			// Apply the updated query string to the URL. For the same reason as
			// the one already explained above, changing the query string here will
			// also change it for all of the following steps of the crawling process.
			ctx.URL().RawQuery = q.Encode()
		} else if e.website.Query.IgnoreAll {
			// If no exception is set, remove all the query string from the URL,
			// but only if IgnoreAll is set to true.
			ctx.URL().RawQuery = ""
		}
	}

	// Check if the fragmentless (and possibly queryless) URL matches the URL of
	// an article that has already been saved in the database. Only check if the
	// URL is in the map, we don't actually care about the value attached.
	_, inMap := e.visitedArticles[ctx.URL().String()]

	// Check if the URL matches with the exclude and restrict filters. To be
	// accepted, a URL must pass the restrict filter and not pass the exclude one.
	// If no filter is specified, we consider the link as passing by default.
	var matchRestrict, matchExclude = true, false
	if e.website.Filters != nil {
		if e.website.Filters.Restrict != nil {
			matchRestrict = e.website.Filters.Restrict.MatchString(ctx.URL().String())
		}
		if e.website.Filters.Exclude != nil {
			matchExclude = e.website.Filters.Exclude.MatchString(ctx.URL().String())
		}
	}

	return !isVisited && !inMap && (matchRestrict && !matchExclude)
}

// Visit implements gocrawl.Extender.Visit
// Parses a web page to check if it contains a news item, and if so extract all
// data available and save it in the database. Also manipulates the content to
// replace all relative links to absolute ones, and to remove <aside> and <script>
// nodes.
// Raises an error (to the parent goroutine) if there was an issue processing the
// item's content (either replacing relative links to absolute ones, or retrieving
// its HTML), parsing the item's date, or saving the item in the database.
func (e *Extender) Visit(ctx *gocrawl.URLContext, res *http.Response, doc *goquery.Document) (interface{}, bool) {
	// Initialise the error that will be raised to the parent goroutine in case
	// it is needed.
	var crawlError = &gocrawl.CrawlError{
		Ctx:  ctx,
		Kind: gocrawl.CekParseBody,
	}

	var err error
	var description, author *string
	var contentNodes, titleNodes, dateNodes *goquery.Selection
	var nodes []*html.Node

	// Find content, title and date using the CSS selectors specified in the
	// configuration file.
	contentNodes = doc.Find(e.website.Selectors.Content)
	titleNodes = doc.Find(e.website.Selectors.Title)
	dateNodes = doc.Find(e.website.Selectors.Date)

	// There should only be one match for content and title. In some weird configurations,
	// there can be more than one match for the date. This is fine as long as there's at
	// least one, only the first match will be used.
	// If one of theses requirements isn't met, it means the page isn't an article.
	if len(contentNodes.Nodes) != 1 || len(titleNodes.Nodes) != 1 || len(dateNodes.Nodes) == 0 {
		e.log.WithFields(logrus.Fields{
			"content_matches": len(contentNodes.Nodes),
			"title_matches":   len(titleNodes.Nodes),
			"date_matches":    len(dateNodes.Nodes),
			"page_url":        ctx.URL().String(),
		}).Debug("Current page isn't an article")

		return nil, true
	}

	// Look for optional data, starting with the description.
	if len(e.website.Selectors.Description) > 0 {
		nodes = doc.Find(e.website.Selectors.Description).Nodes
		if len(nodes) > 0 {
			description = new(string)
			*description = strings.Trim(nodes[0].FirstChild.Data, " \t\n")
		}
	}
	// Search for the post's author.
	if len(e.website.Selectors.Author) > 0 {
		nodes = doc.Find(e.website.Selectors.Author).Nodes
		if len(nodes) > 0 {
			author = new(string)
			if nodes[0].FirstChild.Data == "a" {
				// Sometimes there's a link on the author's name, so we need to
				// go deeper into the children to find the text data.
				*author = strings.Trim(nodes[0].FirstChild.FirstChild.Data, " \t\n")
			} else {
				*author = strings.Trim(nodes[0].FirstChild.Data, " \t\n")
			}

		}
	}
	// Search for the thumbnail. If one is found, add it at the very beginning of
	// the content.
	if len(e.website.Selectors.Thumbnail) > 0 {
		nodes = doc.Find(e.website.Selectors.Thumbnail).Nodes
		if len(nodes) > 0 && nodes[0].Data == "img" {
			contentNodes.PrependNodes(nodes[0])
		}
	}

	// Remove useless content and scripts.
	contentNodes.Find("aside").Remove()
	contentNodes.Find("script").Remove()

	// Make relative links absolute.
	contentNodes.Find("a").Map(func(i int, selection *goquery.Selection) (s string) {
		if err = urlRelativeToAbsolute(selection, doc, "href"); err != nil {
			crawlError.Err = err
			e.Error(crawlError)
		}

		return
	})
	contentNodes.Find("img").Map(func(i int, selection *goquery.Selection) (s string) {
		if err = urlRelativeToAbsolute(selection, doc, "src"); err != nil {
			crawlError.Err = err
			e.Error(crawlError)
		}

		return
	})

	// Extract the HTML content.
	content, err := contentNodes.Html()
	if err != nil {
		crawlError.Err = err
		e.Error(crawlError)
	}

	// Trim unnecessary space, tabs and line breaks.
	title := strings.Trim(titleNodes.Nodes[0].FirstChild.Data, " \t\n")
	date := strings.Trim(dateNodes.Nodes[0].FirstChild.Data, " \t\n")
	// Convert the date into a time.Time instance so it can be stored with a DATE
	// type into PostgreSQL.
	dateTime, err := time.Parse(e.website.DateFormat, date)
	if err != nil {
		crawlError.Err = err
		e.Error(crawlError)
	}

	e.log.WithFields(logrus.Fields{
		"title": title,
		"date":  dateTime.String(),
	}).Info("Saving article")

	// Saving the item in the database.
	if err = e.db.SaveArticle(
		e.website.Identifier, ctx.URL(), title,
		description, content, author, dateTime,
	); err != nil {
		crawlError.Err = err
		e.Error(crawlError)
	}

	return nil, true
}

// Error implements gocrawl.Extender.Error
// Takes a *gocrawl.CrawlError and send the according error message to the parent
// goroutine, according to the data provided.
func (e *Extender) Error(err *gocrawl.CrawlError) {
	if err != nil {
		if err.Ctx == nil {
			if err.Err == nil {
				e.errChan <- fmt.Errorf(
					"Unknown %s error",
					err.Kind.String(),
				)
			} else {
				e.errChan <- fmt.Errorf(
					"%s error: %s",
					err.Kind.String(),
					err.Err.Error(),
				)
			}
		} else if err.Err == nil {
			e.errChan <- fmt.Errorf(
				"Unknown %s error on %s",
				err.Kind.String(),
				err.Ctx.URL().String(),
			)
		} else {
			e.errChan <- fmt.Errorf(
				"%s error on %s: %s",
				err.Kind.String(),
				err.Ctx.URL().String(),
				err.Err.Error(),
			)
		}
	}
}

// Log implements gocrawl.Extender.Log
// Redirects all log to the extender's logger.
func (e *Extender) Log(logFlags gocrawl.LogFlags, msgLevel gocrawl.LogFlags, msg string) {
	if logFlags&msgLevel == msgLevel {
		e.log.Info(msg)
	}
}

// abort tells the parent goroutine to abort the crawling with the given reason.
func (e *Extender) abort(reason string) {
	e.abortChan <- reason
}

// urlRelativeToAbsolute takes a goquery selection referring to a single HTML
// element contaning an URL in one of its attributes, the goquery representation
// of the complete HTML document, and the name of the attribute containing the
// URL in the element, and uses it to replace the attribute's value in the element
// with an absolute URL computed from the document's absolute URL.
func urlRelativeToAbsolute(el *goquery.Selection, doc *goquery.Document, attrName string) (err error) {
	// Extract the relative URL and parse it.
	target, _ := el.Attr(attrName)
	u, err := url.Parse(target)
	if err != nil {
		return
	}

	// Replace the attribute's value in the element.
	el.RemoveAttr(attrName)
	el.SetAttr(attrName, doc.Url.ResolveReference(u).String())

	return
}
