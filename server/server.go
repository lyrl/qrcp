package server

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"github.com/claudiodangelis/qrcp/logger"
	"github.com/claudiodangelis/qrcp/pages"
	"github.com/claudiodangelis/qrcp/payload"
	"github.com/claudiodangelis/qrcp/util"
	"gopkg.in/cheggaaa/pb.v1"
)

// Server is the server
type Server struct {
	// SendURL is the URL used to send the file
	SendURL string
	// ReceiveURL is the URL used to Receive the file
	ReceiveURL  string
	instance    *http.Server
	payload     payload.Payload
	stopchannel chan bool
}

// SetForSend adds a handler for sending the file
func (s *Server) SetForSend(p payload.Payload) error {
	// Add handler
	s.payload = p
	return nil
}

// Wait for transfer to be completed, it waits forever if kept awlive
func (s Server) Wait() error {
	<-s.stopchannel
	if err := s.instance.Shutdown(context.Background()); err != nil {
		log.Println(err)
	}
	if s.payload.DeleteAfterTransfer {
		s.payload.Delete()
	}
	return nil
}

// New instance of the server
func New(iface string, port int) (*Server, error) {
	logger := logger.New()
	app := &Server{}
	// Create the server
	address, err := util.GetInterfaceAddress(iface)
	if err != nil {
		return &Server{}, err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		log.Fatalln(err)
	}
	// TODO: Refactor this
	address = fmt.Sprintf("%s:%d", address, listener.Addr().(*net.TCPAddr).Port)

	randomPath := util.GetRandomURLPath()
	// TODO: Refactor this
	app.SendURL = fmt.Sprintf("http://%s/send/%s",
		listener.Addr().String(), randomPath)
	app.ReceiveURL = fmt.Sprintf("http://%s/receive/%s",
		listener.Addr().String(), randomPath)

	// Create a server
	httpserver := &http.Server{Addr: address}
	// Create channel to send message to stop server
	app.stopchannel = make(chan bool)
	// Create handlers
	// Send handler (sends file to caller)
	// Create cookie used to verify request is coming from first client to connect
	cookie := http.Cookie{Name: "qrcp", Value: ""}
	// Gracefully shutdown when an OS signal is received
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		app.stopchannel <- true
	}()

	// The handler adds and removes from the sync.WaitGroup
	// When the group is zero all requests are completed
	// and the server is shutdown
	// TODO: Refactor this
	var waitgroup sync.WaitGroup
	waitgroup.Add(1)
	var initCookie sync.Once
	http.HandleFunc("/send/"+randomPath, func(w http.ResponseWriter, r *http.Request) {
		if cookie.Value == "" {
			// if !strings.HasPrefix(r.Header.Get("User-Agent"), "Mozilla") {
			// 	http.Error(w, "", http.StatusOK)
			// 	return
			// }
			initCookie.Do(func() {
				value, err := util.GetSessionID()
				if err != nil {
					log.Println("Unable to generate session ID", err)
					app.stopchannel <- true
					return
				}
				cookie.Value = value
				http.SetCookie(w, &cookie)
			})
		} else {
			// Check for the expected cookie and value
			// If it is missing or doesn't match
			// return a 404 status
			rcookie, err := r.Cookie(cookie.Name)
			if err != nil || rcookie.Value != cookie.Value {
				http.Error(w, "", http.StatusNotFound)
				return
			}
			// If the cookie exits and matches
			// this is an aadditional request.
			// Increment the waitgroup
			waitgroup.Add(1)
		}

		defer waitgroup.Done()
		w.Header().Set("Content-Disposition", "attachment; filename="+
			app.payload.Filename)
		http.ServeFile(w, r, app.payload.Path)

	})
	// Upload handler (serves the upload page)
	http.HandleFunc("/receive/"+randomPath, func(w http.ResponseWriter, r *http.Request) {
		// TODO: This can be refactored
		data := struct {
			Route string
			File  string
		}{}
		data.Route = "/receive/" + randomPath
		switch r.Method {
		case "POST":
			// TODO: DO this
			dirToStore := "/tmp"
			filenames := util.ReadFilenames(dirToStore)
			reader, err := r.MultipartReader()
			if err != nil {
				fmt.Fprintf(w, "Upload error: %v\n", err)
				log.Printf("Upload error: %v\n", err)
				app.stopchannel <- true
				return
			}

			transferedFiles := []string{}
			progressBar := pb.New64(r.ContentLength)
			progressBar.ShowCounters = false
			if flag.Lookup("quiet").Value.(flag.Getter).Get().(bool) == true {
				progressBar.NotPrint = true
			}

			for {
				part, err := reader.NextPart()

				if err == io.EOF {
					break
				}
				// if part.FileName() is empty, skip this iteration.
				if part.FileName() == "" {
					continue
				}

				// prepare the dst
				fileName := getFileName(part.FileName(), filenames)
				out, err := os.Create(filepath.Join(dirToStore, fileName))
				if err != nil {
					fmt.Fprintf(w, "Unable to create the file for writing: %s\n", err) //output to server
					log.Printf("Unable to create the file for writing: %s\n", err)     //output to console
					app.stopchannel <- true                                            // send signal to server to shutdown
					return
				}
				defer out.Close()

				// add name of new file
				filenames = append(filenames, fileName)

				// write the content from POSTed file to the out
				logger.Info("Transferring file: ", out.Name())
				progressBar.Prefix(out.Name())
				progressBar.Start()
				buf := make([]byte, 1024)
				for {
					// read a chunk
					n, err := part.Read(buf)
					if err != nil && err != io.EOF {
						fmt.Fprintf(w, "Unable to write file to disk: %v", err) //output to server
						log.Printf("Unable to write file to disk: %v", err)     //output to console
						app.stopchannel <- true                                 // send signal to server to shutdown
						return
					}
					if n == 0 {
						break
					}
					// write a chunk
					if _, err := out.Write(buf[:n]); err != nil {
						fmt.Fprintf(w, "Unable to write file to disk: %v", err) //output to server
						log.Printf("Unable to write file to disk: %v", err)     //output to console
						app.stopchannel <- true                                 // send signal to server to shutdown
						return
					}
					progressBar.Add(n)
				}

				transferedFiles = append(transferedFiles, out.Name())
			}

			progressBar.FinishPrint("File transfer completed")

			data.File = strings.Join(transferedFiles, ", ")
			serveTemplate("done", pages.Done, w, data)
		case "GET":
			serveTemplate("upload", pages.Upload, w, data)
		}
		app.stopchannel <- true
	})
	// Wait for all wg to be done, then send shutdown signal
	go func() {
		waitgroup.Wait()
		app.stopchannel <- true
	}()
	// Receive handler (receives file from caller)
	go func() {
		if err := (httpserver.Serve(tcpKeepAliveListener{listener.(*net.TCPListener)})); err != http.ErrServerClosed {
			log.Fatalln(err)
		}
	}()
	app.instance = httpserver
	return app, nil
}