package main

import (
	"bytes"
	"crypto/md5"
	"encoding/xml"
	"errors"
	"fmt"
	htemplate "html/template"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/air-gases/defibrillator"
	"github.com/air-gases/limiter"
	"github.com/air-gases/logger"
	"github.com/air-gases/redirector"
	"github.com/aofei/air"
	"github.com/fsnotify/fsnotify"
	"github.com/russross/blackfriday/v2"
	"github.com/tdewolff/minify"
	mxml "github.com/tdewolff/minify/xml"
)

type post struct {
	ID       string
	Title    string
	Datetime time.Time
	Content  htemplate.HTML
}

var (
	postsOnce    sync.Once
	posts        map[string]post
	orderedPosts []post

	feed             []byte
	feedTemplate     *template.Template
	feedETag         string
	feedLastModified string
)

func init() {
	postsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(fmt.Errorf("failed to build post watcher: %v", err))
	} else if err := postsWatcher.Add("posts"); err != nil {
		panic(fmt.Errorf("failed to watch post directory: %v", err))
	}

	go func() {
		for {
			select {
			case e := <-postsWatcher.Events:
				air.DEBUG(
					"post file event occurs",
					map[string]interface{}{
						"file":  e.Name,
						"event": e.Op.String(),
					},
				)
				postsOnce = sync.Once{}
			case err := <-postsWatcher.Errors:
				air.ERROR(
					"post watcher error",
					map[string]interface{}{
						"error": err.Error(),
					},
				)
			}
		}
	}()

	b, err := ioutil.ReadFile(filepath.Join(air.TemplateRoot, "feed.xml"))
	if err != nil {
		panic(fmt.Errorf("failed to read feed template file: %v", err))
	}

	feedTemplate = template.Must(
		template.New("feed").
			Funcs(map[string]interface{}{
				"xmlescape": func(s string) string {
					buf := bytes.Buffer{}
					xml.EscapeText(&buf, []byte(s))
					return buf.String()
				},
				"now": func() time.Time {
					return time.Now().UTC()
				},
				"timefmt": air.TemplateFuncMap["timefmt"],
			}).
			Parse(string(b)),
	)
}

func main() {
	air.ErrorHandler = errorHandler
	air.Pregases = []air.Gas{
		logger.Gas(logger.GasConfig{}),
		defibrillator.Gas(defibrillator.GasConfig{}),
		redirector.WWW2NonWWWGas(redirector.WWW2NonWWWGasConfig{}),
		limiter.BodySizeGas(limiter.BodySizeGasConfig{
			MaxBytes: 1 << 20,
			Error413: errors.New("Request Entity Too Large"),
		}),
	}

	air.NotFoundHandler = notFoundHandler
	air.MethodNotAllowedHandler = methodNotAllowedHandler

	air.FILE("/robots.txt", "robots.txt")
	air.STATIC(
		"/assets",
		air.AssetRoot,
		func(next air.Handler) air.Handler {
			return func(req *air.Request, res *air.Response) error {
				res.SetHeader("cache-control", "max-age=3600")
				return next(req, res)
			}
		},
	)
	air.GET("/", homeHandler)
	air.HEAD("/", homeHandler)
	air.GET("/posts", postsHandler)
	air.HEAD("/posts", postsHandler)
	air.GET("/posts/:ID", postHandler)
	air.HEAD("/posts/:ID", postHandler)
	air.GET("/bio", bioHandler)
	air.HEAD("/bio", bioHandler)
	air.GET("/feed", feedHandler)
	air.HEAD("/feed", feedHandler)

	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := air.Serve(); err != nil {
			air.ERROR(
				"server error",
				map[string]interface{}{
					"error": err.Error(),
				},
			)
		}
	}()

	<-shutdownChan
	air.Shutdown(time.Minute)
}

func parsePosts() {
	fns, _ := filepath.Glob("posts/*.md")
	nps := make(map[string]post, len(fns))
	nops := make([]post, 0, len(fns))
	for _, fn := range fns {
		b, _ := ioutil.ReadFile(fn)
		if bytes.Count(b, []byte{'+', '+', '+'}) < 2 {
			continue
		}

		i := bytes.Index(b, []byte{'+', '+', '+'})
		j := bytes.Index(b[i+3:], []byte{'+', '+', '+'}) + 3

		p := post{
			ID:      fn[6 : len(fn)-3],
			Content: htemplate.HTML(blackfriday.Run(b[j+3:])),
		}
		if err := toml.Unmarshal(b[i+3:j], &p); err != nil {
			continue
		}

		p.Datetime = p.Datetime.UTC()

		nps[p.ID] = p
		nops = append(nops, p)
	}

	sort.Slice(nops, func(i, j int) bool {
		return nops[i].Datetime.After(nops[j].Datetime)
	})

	posts = nps
	orderedPosts = nops

	latestPosts := orderedPosts
	if len(latestPosts) > 10 {
		latestPosts = latestPosts[:10]
	}

	buf := bytes.Buffer{}
	feedTemplate.Execute(&buf, map[string]interface{}{
		"Posts": latestPosts,
	})

	buf2 := bytes.Buffer{}
	mxml.DefaultMinifier.Minify(minify.New(), &buf2, &buf, nil)

	if b := buf2.Bytes(); !bytes.Equal(b, feed) {
		feed = b
		feedETag = fmt.Sprintf(`"%x"`, md5.Sum(feed))
		feedLastModified = time.Now().UTC().Format(http.TimeFormat)
	}
}

func homeHandler(req *air.Request, res *air.Response) error {
	req.Values["CanonicalPath"] = ""
	return res.Render(req.Values, "index.html")
}

func postsHandler(req *air.Request, res *air.Response) error {
	postsOnce.Do(parsePosts)
	req.Values["PageTitle"] = req.LocalizedString("Posts")
	req.Values["CanonicalPath"] = "/posts"
	req.Values["IsPosts"] = true
	req.Values["Posts"] = orderedPosts
	return res.Render(req.Values, "posts.html", "layouts/default.html")
}

func postHandler(req *air.Request, res *air.Response) error {
	postsOnce.Do(parsePosts)

	p, ok := posts[req.Param("ID").Value().String()]
	if !ok {
		return air.NotFoundHandler(req, res)
	}

	req.Values["PageTitle"] = p.Title
	req.Values["CanonicalPath"] = "/posts/" + p.ID
	req.Values["IsPosts"] = true
	req.Values["Post"] = p

	return res.Render(req.Values, "post.html", "layouts/default.html")
}

func bioHandler(req *air.Request, res *air.Response) error {
	req.Values["PageTitle"] = req.LocalizedString("Bio")
	req.Values["CanonicalPath"] = "/bio"
	req.Values["IsBio"] = true
	return res.Render(req.Values, "bio.html", "layouts/default.html")
}

func feedHandler(req *air.Request, res *air.Response) error {
	postsOnce.Do(parsePosts)

	res.SetHeader("content-type", "application/atom+xml; charset=utf-8")
	res.SetHeader("cache-control", "max-age=3600")
	res.SetHeader("etag", feedETag)
	res.SetHeader("last-modified", feedLastModified)

	return res.WriteBlob(feed)
}

func errorHandler(err error, req *air.Request, res *air.Response) {
	if res.Written {
		return
	}

	if res.Status < 400 {
		res.Status = 500
	}

	message := err.Error()
	if res.Status == 500 && !air.DebugMode {
		message = "Internal Server Error"
	}

	if req.Method == "GET" || req.Method == "HEAD" {
		res.SetHeader("cache-control")
		res.SetHeader("etag")
		res.SetHeader("last-modified")
	}

	req.Values["PageTitle"] = res.Status
	req.Values["Error"] = map[string]interface{}{
		"Code":    res.Status,
		"Message": message,
	}

	res.Render(req.Values, "error.html", "layouts/default.html")
}

var notFoundHandler = func(req *air.Request, res *air.Response) error {
	res.Status = 404
	return errors.New("Not Found")
}

var methodNotAllowedHandler = func(req *air.Request, res *air.Response) error {
	res.Status = 405
	return errors.New("Method Not Allowed")
}
