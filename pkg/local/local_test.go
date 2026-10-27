/*
MIT License

Copyright (c) 2024 Jonas Kaninda

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

package local

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const content = "Lorem ipsum dolor sit amet. Eum eius voluptas sit vitae vitae aut sequi molestias hic accusamus consequatur"
const inputFile = "file.txt"
const localPath = "./tests/local"
const RemotePath = "./tests/remote"

func TestCopy(t *testing.T) {

	err := os.MkdirAll(localPath, 0777)
	if err != nil {
		t.Error(err)
	}
	err = os.MkdirAll(RemotePath, 0777)
	if err != nil {
		t.Error(err)
	}

	_, err = createFile(filepath.Join(localPath, inputFile), content)
	if err != nil {
		t.Error(err)
	}

	l := NewStorage(Config{
		LocalPath:  "./tests/local",
		RemotePath: "./tests/remote",
	})
	err = l.Copy(inputFile)
	if err != nil {
		t.Error(err)
	}
	fmt.Printf("File copied to %s\n", filepath.Join(RemotePath, inputFile))
}
func createFile(fileName, content string) ([]byte, error) {
	// Create a file named hello.txt
	file, err := os.Create(fileName)
	if err != nil {
		fmt.Println("Error creating file:", err)
		return nil, err
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			return
		}
	}(file)

	// Write the message to the file
	_, err = file.WriteString(content)
	if err != nil {
		fmt.Println("Error writing to file:", err)
		return nil, err
	}

	fmt.Printf("Successfully wrote to %s\n", fileName)
	fileBytes, err := os.ReadFile(fileName)
	return fileBytes, err
}
