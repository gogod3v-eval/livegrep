package server

import (
	"code.google.com/p/go.net/websocket"
	"fmt"
	"github.com/nelhage/livegrep/client"
	"log"
)

type searchConnection struct {
	srv      *server
	ws       *websocket.Conn
	backend  string
	client   client.Client
	errors   chan error
	incoming chan Op
	outgoing chan Op
	shutdown bool
}

func (s *searchConnection) recvLoop() {
	var op Op
	for {
		if err := OpCodec.Receive(s.ws, &op); err != nil {
			log.Printf("Error in receive: %s\n", err.Error())
			if _, ok := err.(*ProtocolError); ok {
				// TODO: is this a good idea?
				// s.outgoing <- &OpError{err.Error()}
				continue
			} else {
				s.errors <- err
				break
			}
		}
		log.Printf("Incoming: %+v", op)
		s.incoming <- op
		if s.shutdown {
			break
		}
	}
	close(s.incoming)
}

func (s *searchConnection) sendLoop() {
	for op := range s.outgoing {
		log.Printf("Outgoing: %+v", op)
		OpCodec.Send(s.ws, op)
	}
}

func query(q *OpQuery) *client.Query {
	return &client.Query{
		Line: q.Line,
		File: q.File,
		Repo: q.Repo,
	}
}

func (s *searchConnection) handle() {
	s.incoming = make(chan Op, 1)
	s.outgoing = make(chan Op, 1)
	s.errors = make(chan error, 1)

	go s.recvLoop()
	go s.sendLoop()
	defer close(s.outgoing)

	var nextQuery *OpQuery
	var inFlight *OpQuery

	var search client.Search
	var results <-chan *client.Result
	var err error

SearchLoop:
	for {
		select {
		case op, ok := <-s.incoming:
			if !ok {
				break
			}
			switch t := op.(type) {
			case *OpQuery:
				nextQuery = t
			default:
				s.outgoing <- &OpError{fmt.Sprintf("Invalid opcode %s", op.Opcode())}
				break
			}

		case e := <-s.errors:
			log.Printf("error reading from client: %s\n", e.Error())
			break SearchLoop
		case res, ok := <-results:
			if ok {
				s.outgoing <- &OpResult{inFlight.Id, res}
			} else {
				st, err := search.Close()
				if err == nil {
					s.outgoing <- &OpSearchDone{inFlight.Id, st}
				} else {
					s.outgoing <- &OpQueryError{inFlight.Id, err.Error()}
				}
				results = nil
				search = nil
				inFlight = nil
			}
		}
		if nextQuery != nil && results == nil {
			if err := s.connectBackend(nextQuery.Backend); err != nil {
				s.outgoing <- &OpQueryError{nextQuery.Id, err.Error()}
				nextQuery = nil
				continue
			}
			search, err = s.client.Query(query(nextQuery))
			if err != nil {
				s.outgoing <- &OpQueryError{nextQuery.Id, err.Error()}
			} else {
				if search == nil {
					panic("nil search and nil error?")
				}
				inFlight = nextQuery
				results = search.Results()
			}
			nextQuery = nil
		}
	}

	s.shutdown = true
}

func (s *searchConnection) connectBackend(backend string) error {
	if s.client == nil || s.backend != backend {
		if s.client != nil {
			s.client.Close()
		}
		s.backend = backend
		addr := ""
		for _, bk := range s.srv.config.Backends {
			if bk.Id == backend {
				addr = bk.Addr
				break
			}
		}
		if addr == "" {
			return fmt.Errorf("No such backend: %s", backend)
		}
		s.client = client.ClientWithRetry(func() (client.Client, error) {
			return client.Dial("tcp", addr)
		})
	}
	return nil
}

func (s *server) HandleWebsocket(ws *websocket.Conn) {
	c := &searchConnection{
		srv: s,
		ws:  ws,
	}
	c.handle()
}
