// Copyright 2026 The Outband Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
)

func main() {
	target := flag.String("target", "", "upstream API URL (e.g., https://api.openai.com)")
	listen := flag.String("listen", "localhost:8080", "address to listen on")
	flag.Parse()

	if *target == "" {
		fmt.Fprintln(os.Stderr, "error: --target is required")
		flag.Usage()
		os.Exit(1)
	}

	targetURL, err := url.Parse(*target)
	if err != nil {
		log.Fatalf("invalid target URL: %v", err)
	}

	// Validate scheme
	if targetURL.Scheme != "http" && targetURL.Scheme != "https" {
		log.Fatalf("target URL must have http or https scheme, got %q", targetURL.Scheme)
	}

	_ = listen
	_ = targetURL
}
