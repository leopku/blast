// Copyright (c) 2019 Minoru Osuka
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package indexer

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/blevesearch/bleve"
	"github.com/hashicorp/raft"
	raftbadgerdb "github.com/markthethomas/raft-badger"
	_ "github.com/mosuka/blast/config"
	blasterrors "github.com/mosuka/blast/errors"
	"go.uber.org/zap"
	//raftmdb "github.com/hashicorp/raft-mdb"
)

type RaftServer struct {
	id       string
	metadata map[string]interface{}

	bootstrap bool

	raft *raft.Raft
	fsm  *RaftFSM

	indexConfig map[string]interface{}

	logger *zap.Logger
}

func NewRaftServer(id string, metadata map[string]interface{}, bootstrap bool, indexConfig map[string]interface{}, logger *zap.Logger) (*RaftServer, error) {
	return &RaftServer{
		id:       id,
		metadata: metadata,

		bootstrap: bootstrap,

		indexConfig: indexConfig,
		logger:      logger,
	}, nil
}

func (s *RaftServer) Start() error {
	var err error

	dataDir, ok := s.metadata["data_dir"].(string)
	if !ok {
		s.logger.Fatal("missing metadata", zap.String("data_dir", dataDir))
		return errors.New("missing metadata")
	}

	bindAddr, ok := s.metadata["bind_addr"].(string)
	if !ok {
		s.logger.Fatal("missing metadata", zap.String("bind_addr", bindAddr))
		return errors.New("missing metadata")
	}

	fsmPath := filepath.Join(dataDir, "index")
	s.logger.Info("create finite state machine", zap.String("path", fsmPath))
	s.fsm, err = NewRaftFSM(fsmPath, s.indexConfig, s.logger)
	if err != nil {
		s.logger.Fatal(err.Error())
		return err
	}

	s.logger.Info("start finite state machine")
	err = s.fsm.Start()
	if err != nil {
		s.logger.Fatal(err.Error())
		return err
	}

	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(s.id)
	config.SnapshotThreshold = 1024
	config.LogOutput = ioutil.Discard

	s.logger.Info("resolve TCP address", zap.String("address", bindAddr))
	addr, err := net.ResolveTCPAddr("tcp", bindAddr)
	if err != nil {
		s.logger.Fatal(err.Error())
		return err
	}

	s.logger.Info("create TCP transport", zap.String("bind_addr", bindAddr))
	transport, err := raft.NewTCPTransport(bindAddr, addr, 3, 10*time.Second, ioutil.Discard)
	if err != nil {
		s.logger.Fatal(err.Error())
		return err
	}

	snapshotPath := filepath.Join(dataDir, "snapshots")
	s.logger.Info("create snapshot store", zap.String("path", snapshotPath))
	snapshotStore, err := raft.NewFileSnapshotStore(snapshotPath, 2, ioutil.Discard)
	if err != nil {
		s.logger.Fatal(err.Error())
		return err
	}

	logStore := filepath.Join(dataDir, "raft.db")
	s.logger.Info("create Raft log store", zap.String("path", logStore))
	//raftLogStore, err := raftboltdb.NewBoltStore(logStore)
	err = os.MkdirAll(filepath.Join(logStore, "badger"), 0755)
	if err != nil {
		s.logger.Fatal(err.Error())
		return err
	}
	raftLogStore, err := raftbadgerdb.NewBadgerStore(logStore)
	//raftLogStore, err := raftmdb.NewMDBStore(logStore)
	if err != nil {
		s.logger.Fatal(err.Error())
		return err
	}

	s.logger.Info("create Raft machine")
	s.raft, err = raft.NewRaft(config, s.fsm, raftLogStore, raftLogStore, snapshotStore, transport)
	if err != nil {
		s.logger.Fatal(err.Error())
		return err
	}

	if s.bootstrap {
		s.logger.Info("configure Raft machine as bootstrap")
		configuration := raft.Configuration{
			Servers: []raft.Server{
				{
					ID:      config.LocalID,
					Address: transport.LocalAddr(),
				},
			},
		}
		s.raft.BootstrapCluster(configuration)

		s.logger.Info("wait for become a leader")
		err = s.WaitForDetectLeader(60 * time.Second)
		if err != nil {
			s.logger.Fatal(err.Error())
			return err
		}

		// set metadata
		s.logger.Info("register its own information", zap.String("id", s.id), zap.Any("metadata", s.metadata))
		err = s.setMetadata(s.id, s.metadata)
		if err != nil {
			s.logger.Fatal(err.Error())
			return nil
		}
	}

	return nil
}

func (s *RaftServer) Stop() error {
	s.logger.Info("shutdown Raft machine")
	f := s.raft.Shutdown()
	err := f.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	s.logger.Info("stop finite state machine")
	err = s.fsm.Stop()
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	return nil
}

func (s *RaftServer) LeaderAddress(timeout time.Duration) (raft.ServerAddress, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			leaderAddr := s.raft.Leader()
			if leaderAddr != "" {
				s.logger.Debug("detect a leader", zap.String("address", string(leaderAddr)))
				return leaderAddr, nil
			}
		case <-timer.C:
			s.logger.Error("timeout exceeded")
			return "", blasterrors.ErrTimeout
		}
	}
}

func (s *RaftServer) LeaderID(timeout time.Duration) (raft.ServerID, error) {
	leaderAddr, err := s.LeaderAddress(timeout)
	if err != nil {
		s.logger.Error(err.Error())
		return "", err
	}

	cf := s.raft.GetConfiguration()
	err = cf.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return "", err
	}

	for _, server := range cf.Configuration().Servers {
		if server.Address == leaderAddr {
			return server.ID, nil
		}
	}

	s.logger.Error(blasterrors.ErrNotFoundLeader.Error())
	return "", blasterrors.ErrNotFoundLeader
}

func (s *RaftServer) Stats() map[string]string {
	return s.raft.Stats()
}

func (s *RaftServer) State() string {
	return s.raft.State().String()
}

func (s *RaftServer) IsLeader() bool {
	return s.raft.State() == raft.Leader
}

func (s *RaftServer) WaitForDetectLeader(timeout time.Duration) error {
	_, err := s.LeaderAddress(timeout)
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	return nil
}

func (s *RaftServer) getMetadata(id string) (map[string]interface{}, error) {
	metadata, err := s.fsm.GetMetadata(id)
	if err != nil {
		s.logger.Error(err.Error())
		return nil, err
	}

	return metadata, nil
}

func (s *RaftServer) setMetadata(id string, metadata map[string]interface{}) error {
	s.logger.Debug("set metadata", zap.String("id", id), zap.Any("metadata", metadata))

	msg, err := newMessage(
		setNode,
		map[string]interface{}{
			"id":       id,
			"metadata": metadata,
		},
	)
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	f := s.raft.Apply(msgBytes, 10*time.Second)
	err = f.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}
	err = f.Response().(*fsmResponse).error
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	return nil
}

func (s *RaftServer) deleteMetadata(id string) error {
	s.logger.Debug("delete metadata", zap.String("id", id))

	msg, err := newMessage(
		deleteNode,
		map[string]interface{}{
			"id": id,
		},
	)
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	f := s.raft.Apply(msgBytes, 10*time.Second)
	err = f.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}
	err = f.Response().(*fsmResponse).error
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	return nil
}

func (s *RaftServer) GetMetadata(id string) (map[string]interface{}, error) {
	s.logger.Debug("get metadata", zap.String("id", id))

	cf := s.raft.GetConfiguration()
	err := cf.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return nil, err
	}

	var metadata map[string]interface{}
	for _, server := range cf.Configuration().Servers {
		if server.ID == raft.ServerID(id) {
			metadata, err = s.getMetadata(id)
			if err != nil {
				s.logger.Error(err.Error())
				return nil, err
			}
			break
		}
	}

	return metadata, nil
}

func (s *RaftServer) SetMetadata(id string, metadata map[string]interface{}) error {
	s.logger.Info("set metadata", zap.String("id", id), zap.Any("metadata", metadata))

	if !s.IsLeader() {
		s.logger.Warn(raft.ErrNotLeader.Error(), zap.String("state", s.raft.State().String()))
		return raft.ErrNotLeader
	}

	cf := s.raft.GetConfiguration()
	err := cf.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	for _, server := range cf.Configuration().Servers {
		if server.ID == raft.ServerID(id) {
			s.logger.Info("node already joined the cluster", zap.String("id", id))
			return nil
		}
	}

	bindAddr, ok := metadata["bind_addr"].(string)
	if !ok {
		s.logger.Error("missing metadata", zap.String("bind_addr", bindAddr))
		return errors.New("missing metadata")
	}

	s.logger.Info("add voter", zap.String("id", id), zap.String("address", bindAddr))
	f := s.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(bindAddr), 0, 0)
	err = f.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	// set metadata
	err = s.setMetadata(id, metadata)
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	return nil
}

func (s *RaftServer) DeleteMetadata(id string) error {
	s.logger.Info("delete metadata", zap.String("id", id))

	if !s.IsLeader() {
		s.logger.Warn(raft.ErrNotLeader.Error(), zap.String("state", s.raft.State().String()))
		return raft.ErrNotLeader
	}

	cf := s.raft.GetConfiguration()
	err := cf.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	for _, server := range cf.Configuration().Servers {
		if server.ID == raft.ServerID(id) {
			s.logger.Debug("remove server", zap.String("id", id))
			f := s.raft.RemoveServer(server.ID, 0, 0)
			err = f.Error()
			if err != nil {
				s.logger.Error(err.Error())
				return err
			}
		}
	}

	// delete metadata
	err = s.deleteMetadata(id)
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	return nil
}

func (s *RaftServer) GetServers() (map[string]interface{}, error) {
	cf := s.raft.GetConfiguration()
	err := cf.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return nil, err
	}

	servers := map[string]interface{}{}
	for _, server := range cf.Configuration().Servers {
		metadata, err := s.GetMetadata(string(server.ID))
		if err != nil {
			s.logger.Warn(err.Error())
		}
		servers[string(server.ID)] = metadata
	}

	return servers, nil
}

func (s *RaftServer) Snapshot() error {
	f := s.raft.Snapshot()
	err := f.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return err
	}

	return nil
}

func (s *RaftServer) GetDocument(id string) (map[string]interface{}, error) {
	fields, err := s.fsm.GetDocument(id)
	if err != nil {
		s.logger.Error(err.Error())
		return nil, err
	}

	return fields, nil
}

func (s *RaftServer) Search(request *bleve.SearchRequest) (*bleve.SearchResult, error) {
	result, err := s.fsm.Search(request)
	if err != nil {
		s.logger.Error(err.Error())
		return nil, err
	}

	return result, nil
}

func (s *RaftServer) IndexDocument(docs []map[string]interface{}) (int, error) {
	if !s.IsLeader() {
		s.logger.Error(raft.ErrNotLeader.Error(), zap.String("state", s.raft.State().String()))
		return -1, raft.ErrNotLeader
	}

	msg, err := newMessage(
		indexDocument,
		docs,
	)
	if err != nil {
		s.logger.Error(err.Error())
		return -1, err
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		s.logger.Error(err.Error())
		return -1, err
	}

	f := s.raft.Apply(msgBytes, 10*time.Second)
	err = f.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return -1, err
	}
	err = f.Response().(*fsmIndexDocumentResponse).error
	if err != nil {
		s.logger.Error(err.Error())
		return -1, err
	}

	return f.Response().(*fsmIndexDocumentResponse).count, nil
}

func (s *RaftServer) DeleteDocument(ids []string) (int, error) {
	if !s.IsLeader() {
		s.logger.Error(raft.ErrNotLeader.Error(), zap.String("state", s.raft.State().String()))
		return -1, raft.ErrNotLeader
	}

	msg, err := newMessage(
		deleteDocument,
		ids,
	)
	if err != nil {
		s.logger.Error(err.Error())
		return -1, err
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		s.logger.Error(err.Error())
		return -1, err
	}

	f := s.raft.Apply(msgBytes, 10*time.Second)
	err = f.Error()
	if err != nil {
		s.logger.Error(err.Error())
		return -1, err
	}
	err = f.Response().(*fsmDeleteDocumentResponse).error
	if err != nil {
		s.logger.Error(err.Error())
		return -1, err
	}

	return f.Response().(*fsmDeleteDocumentResponse).count, nil
}

func (s *RaftServer) GetIndexConfig() (map[string]interface{}, error) {
	indexConfig, err := s.fsm.GetIndexConfig()
	if err != nil {
		s.logger.Error(err.Error())
		return nil, err
	}

	return indexConfig, nil
}

func (s *RaftServer) GetIndexStats() (map[string]interface{}, error) {
	indexStats, err := s.fsm.GetIndexStats()
	if err != nil {
		s.logger.Error(err.Error())
		return nil, err
	}

	return indexStats, nil
}
