/*
MIT License

Copyright (c) 2023 Jonas Kaninda

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package ssh

import (
	"context"
	"errors"
	"fmt"
	"github.com/bramvdbogaerde/go-scp"
	"github.com/bramvdbogaerde/go-scp/auth"
	"github.com/jkaninda/go-storage/pkg"
	"golang.org/x/crypto/ssh"
	"os"
	"path/filepath"
)

type sshStorage struct {
	*pkg.Backend
	client scp.Client
}

// Config holds the SSH connection details
type Config struct {
	Host         string
	User         string
	Password     string
	Port         string
	IdentifyFile string
	LocalPath    string
	RemotePath   string
}

// createClient creates SSH Client
func createClient(conf Config) (scp.Client, error) {
	if _, err := os.Stat(conf.IdentifyFile); os.IsNotExist(err) {
		clientConfig, err := auth.PrivateKey(conf.User, conf.IdentifyFile, ssh.InsecureIgnoreHostKey())
		return scp.NewClient(fmt.Sprintf("%s:%s", conf.Host, conf.Port), &clientConfig), err
	} else {
		if conf.Password == "" {
			return scp.Client{}, errors.New("ssh password required")
		}
		clientConfig, err := auth.PasswordKey(conf.User, conf.Password, ssh.InsecureIgnoreHostKey())
		return scp.NewClient(fmt.Sprintf("%s:%s", conf.Host, conf.Port), &clientConfig), err

	}
}

// NewStorage creates new Storage
func NewStorage(conf Config) (pkg.Storage, error) {
	client, err := createClient(conf)
	if err != nil {
		return nil, err
	}
	return &sshStorage{
		client: client,
		Backend: &pkg.Backend{
			RemotePath: conf.RemotePath,
			LocalPath:  conf.LocalPath,
		},
	}, nil
}

// Copy copies file to the remote server
func (s sshStorage) Copy(fileName string) error {
	client := s.client
	// Connect to the remote server
	err := client.Connect()
	if err != nil {
		return errors.New("couldn't establish a connection to the remote server")
	}
	// Open the local file
	filePath := filepath.Join(s.LocalPath, fileName)
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer client.Close()
	// Copy file to the remote server
	err = client.CopyFromFile(context.Background(), *file, filepath.Join(s.RemotePath, fileName), "0655")
	if err != nil {
		return fmt.Errorf("failed to copy file to remote server: %w", err)
	}

	return nil
}

// CopyFrom copies a file from the remote server to local storage
func (s sshStorage) CopyFrom(fileName string) error {
	// Create a new SCP client
	client := s.client
	// Connect to the remote server
	err := client.Connect()
	if err != nil {
		return errors.New("couldn't establish a connection to the remote server")
	}
	// Close client connection after the file has been copied
	defer client.Close()
	file, err := os.OpenFile(filepath.Join(s.LocalPath, fileName), os.O_RDWR|os.O_CREATE, 0777)
	if err != nil {
		return errors.New("couldn't open the output file")
	}
	defer file.Close()

	err = client.CopyFromRemote(context.Background(), file, filepath.Join(s.RemotePath, fileName))

	if err != nil {
		return err
	}
	return nil
}

// Prune deletes old backup created more than specified days
func (s sshStorage) Prune(retentionDays int) error {
	fmt.Println("Deleting old backup from a remote server is not implemented yet")
	return nil
}

// Name returns the storage name
func (s sshStorage) Name() string {
	return "ssh"
}
