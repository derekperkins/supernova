package supernova

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"mime"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/valyala/fasthttp"
)

// A SuperNova represents the router and all associated data
type SuperNova struct {
	server *fasthttp.Server
	ln     net.Listener

	// radix tree for looking up routes
	paths map[string]*Node

	staticDirs         []string
	middleWare         []MiddleWare
	cachedStatic       *CachedStatic
	maxCachedTime      int64
	compressionEnabled bool

	// shutdown function called when ctl-c is intercepted
	shutdownHandler func()
}

type Node struct {
	route    *Route
	isEdge   bool
	children map[string]*Node
}

type CachedObj struct {
	data       []byte
	timeCached time.Time
}

type CachedStatic struct {
	mutex sync.Mutex
	files map[string]*CachedObj
}

//Middleware obj to hold functions
type MiddleWare struct {
	middleFunc func(*Request, func())
}

func Super() *SuperNova {
	s := new(SuperNova)
	s.cachedStatic = new(CachedStatic)
	s.cachedStatic.files = make(map[string]*CachedObj)
	return s
}

// Serve starts the server
func (sn *SuperNova) Serve(addr string) error {
	sn.server = &fasthttp.Server{
		Handler: sn.handler,
	}
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		return err
	}

	sn.ln = NewGracefulListener(listener, time.Second*5)

	return sn.server.Serve(sn.ln)
}

// ServeTLS starts server with ssl
func (sn *SuperNova) ServeTLS(addr, certFile, keyFile string) error {
	return fasthttp.ListenAndServeTLS(addr, certFile, keyFile, sn.handler)
}

// handler is the main entry point into the router
func (sn *SuperNova) handler(ctx *fasthttp.RequestCtx) {
	request := NewRequest(ctx)

	//Run Middleware
	finished := sn.runMiddleware(request)
	if !finished {
		return
	}

	route := sn.climbTree(request.GetMethod(), request.BaseUrl)
	if route != nil {
		route.call(request)
		return
	}

	//Check for static file
	served := sn.serveStatic(request)
	if served {
		return
	}

	ctx.Error("404 Not Found", fasthttp.StatusNotFound)
}

// All adds route for all http methods
func (sn *SuperNova) All(route string, routeFunc func(*Request)) {
	routeObj := buildRoute(route, routeFunc)
	sn.addRoute("", routeObj)
}

// Get adds only GET method to route
func (sn *SuperNova) Get(route string, routeFunc func(*Request)) {
	routeObj := buildRoute(route, routeFunc)
	sn.addRoute("GET", routeObj)
}

// Post adds only POST method to route
func (sn *SuperNova) Post(route string, routeFunc func(*Request)) {
	routeObj := buildRoute(route, routeFunc)
	sn.addRoute("POST", routeObj)
}

// Put adds only PUT method to route
func (sn *SuperNova) Put(route string, routeFunc func(*Request)) {
	routeObj := buildRoute(route, routeFunc)
	sn.addRoute("PUT", routeObj)
}

// Delete adds only DELETE method to route
func (sn *SuperNova) Delete(route string, routeFunc func(*Request)) {
	routeObj := buildRoute(route, routeFunc)
	sn.addRoute("DELETE", routeObj)
}

// addRoute takes route and method and adds it to route tree
func (sn *SuperNova) addRoute(method string, route *Route) {
	routeStr := route.route
	if routeStr[len(routeStr)-1] == '/' {
		routeStr = routeStr[:len(routeStr)-1]
		route.route = routeStr
	}
	if sn.paths == nil {
		sn.paths = make(map[string]*Node)
	}

	if sn.paths[method] == nil {
		node := new(Node)
		node.children = make(map[string]*Node)
		sn.paths[method] = node
	}

	parts := strings.Split(routeStr[1:], "/")

	currentNode := sn.paths[method]
	for index, val := range parts {
		if val[0] == ':' {
			node := getNode(false, nil)
			currentNode.children[""] = node
			currentNode = node
		} else {
			node := getNode(false, nil)
			currentNode.children[val] = node
			currentNode = node
		}

		if index == len(parts)-1 {
			node := getNode(true, route)
			currentNode.children[val] = node
			currentNode = node
		}
	}
}

func getNode(isEdge bool, route *Route) *Node {
	node := new(Node)
	node.children = make(map[string]*Node)
	if isEdge {
		node.isEdge = true
		node.route = route
	}
	return node
}

// climbTree takes in path and traverses tree to find route
func (sn *SuperNova) climbTree(method, path string) *Route {
	parts := strings.Split(path[1:], "/")
	pathLen := len(parts) - 1

	currentNode := sn.paths[method]
	for index, val := range parts {
		var node *Node
		node = currentNode.children[val]
		if node == nil {
			node = currentNode.children[""]
		}

		if node != nil {
			currentNode = node
		}

		if index == pathLen {
			return currentNode.children[val].route
		}
	}

	return nil
}

// buildRoute creates new Route
func buildRoute(route string, routeFunc func(*Request)) *Route {
	routeObj := new(Route)
	routeObj.routeFunc = routeFunc

	routeObj.routeParamsIndex = make(map[int]string)

	routeObj.route = route

	return routeObj
}

// AddStatic adds static route to be served
func (sn *SuperNova) AddStatic(dir string) {
	if sn.staticDirs == nil {
		sn.staticDirs = make([]string, 0)
	}

	if _, err := os.Stat(dir); err == nil {
		sn.staticDirs = append(sn.staticDirs, dir)
	}
}

// EnableGzip turns on Gzip compression for static
func (sn *SuperNova) EnableGzip(value bool) {
	sn.compressionEnabled = value
}

// serveStatic looks up folder and serves static files
func (sn *SuperNova) serveStatic(req *Request) bool {
	for i := range sn.staticDirs {
		staticDir := sn.staticDirs[i]
		path := staticDir + string(req.Request.RequestURI())

		//Remove all .. for security TODO: Allow if doesn't go above basedir
		path = strings.Replace(path, "..", "", -1)

		//If ends in / default to index.html
		if strings.HasSuffix(path, "/") {
			path += "index.html"
		}

		if stat, err := os.Stat(path); err == nil {
			//Set mime type
			extensionParts := strings.Split(path, ".")
			ext := extensionParts[len(extensionParts)-1]
			mType := mime.TypeByExtension("." + ext)

			if mType != "" {
				req.Response.Header.Set("Content-Type", mType)
			}

			if sn.compressionEnabled && stat.Size() < 10000000 {
				var b bytes.Buffer
				writer := gzip.NewWriter(&b)

				data, err := ioutil.ReadFile(path)
				if err != nil {
					println("Unable to read: " + err.Error())
				}

				writer.Write(data)
				writer.Close()
				req.Response.Header.Set("Content-Encoding", "gzip")
				req.Send(b.String())
			} else {
				req.Response.SendFile(path)
			}

			return true
		}
	}
	return false
}

//Adds a new function to the middleware stack
func (sn *SuperNova) Use(f func(*Request, func())) {
	if sn.middleWare == nil {
		sn.middleWare = make([]MiddleWare, 0)
	}
	middle := new(MiddleWare)
	middle.middleFunc = f
	sn.middleWare = append(sn.middleWare, *middle)
}

//Internal method that runs the middleware
func (sn *SuperNova) runMiddleware(req *Request) bool {
	stackFinished := true
	for m := range sn.middleWare {
		nextCalled := false
		sn.middleWare[m].middleFunc(req, func() {
			nextCalled = true
		})

		if !nextCalled {
			stackFinished = false
			break
		}
	}

	return stackFinished
}

func (sn *SuperNova) SetShutDownHandler(shutdownFunc func()) {
	sigs := make(chan os.Signal)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-sigs:
			println("Gracefully finishing routes before exiting")
			err := sn.ln.Close()
			if err != nil {
				fmt.Printf("Error closing conn: %s\n", err.Error())
			}

			if shutdownFunc != nil {
				shutdownFunc()
			}
			os.Exit(0)
		}
	}
}
