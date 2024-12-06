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

package ftp

import (
	"fmt"
	"github.com/jkaninda/go-storage/pkg"
	"github.com/jlaffaye/ftp"
	"io"
	"os"
	"path/filepath"
	"time"
)

type ftpStorage struct {
	*pkg.Backend
	client *ftp.ServerConn
}

// Config holds the SSH connection details
type Config struct {
	Host       string
	User       string
	Password   string
	Port       string
	LocalPath  string
	RemotePath string
}

// createClient creates FTP Client
func createClient(conf Config) (*ftp.ServerConn, error) {
	ftpClient, err := ftp.Dial(fmt.Sprintf("%s:%s", conf.Host, conf.Port), ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to FTP: %w", err)
	}

	err = ftpClient.Login(conf.User, conf.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to log in to FTP: %w", err)
	}

	return ftpClient, nil
}

// NewStorage creates new Storage
func NewStorage(conf Config) (pkg.Storage, error) {
	client, err := createClient(conf)
	if err != nil {
		return nil, err
	}
	return &ftpStorage{
		client: client,
		Backend: &pkg.Backend{
			RemotePath: conf.RemotePath,
			LocalPath:  conf.LocalPath,
		},
	}, nil
}

// Copy copies file to the remote server
func (s ftpStorage) Copy(fileName string) error {
	ftpClient := s.client
	defer ftpClient.Quit()

	filePath := filepath.Join(s.LocalPath, fileName)
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", fileName, err)
	}
	defer file.Close()

	remoteFilePath := filepath.Join(s.RemotePath, fileName)
	err = ftpClient.Stor(remoteFilePath, file)
	if err != nil {
		return fmt.Errorf("failed to upload file %s: %w", filepath.Join(s.LocalPath, fileName), err)
	}

	return nil
}

// CopyFrom copies a file from the remote server to local storage
func (s ftpStorage) CopyFrom(fileName string) error {
	ftpClient := s.client

	defer ftpClient.Quit()

	remoteFilePath := filepath.Join(s.RemotePath, fileName)
	r, err := ftpClient.Retr(remoteFilePath)
	if err != nil {
		return fmt.Errorf("failed to retrieve file %s: %w", fileName, err)
	}
	defer r.Close()

	localFilePath := filepath.Join(s.LocalPath, fileName)
	outFile, err := os.Create(localFilePath)
	if err != nil {
		return fmt.Errorf("failed to create local file %s: %w", fileName, err)
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, r)
	if err != nil {
		return fmt.Errorf("failed to copy data to local file %s: %w", fileName, err)
	}

	return nil
}

// Prune deletes old backup created more than specified days
func (s ftpStorage) Prune(retentionDays int) error {
	fmt.Println("Deleting old backup from a remote server is not implemented yet")
	return nil

}

// Name returns the storage name
func (s ftpStorage) Name() string {
	return "ftp"
}
