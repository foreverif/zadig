/*
Copyright 2021 The KodeRover Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package handler

import (
	"path/filepath"

	"github.com/gin-gonic/gin"

	"github.com/koderover/zadig/pkg/config"
	"github.com/koderover/zadig/pkg/microservice/policy/core/service/bundle"
)

func DownloadBundle(c *gin.Context) {
	revision := bundle.GetRevision()
	matching := c.GetHeader("If-None-Match")
	if revision != "" && revision == matching {
		c.Status(304)
		return
	}

	c.Header("Content-Type", "application/gzip")
	if revision != "" {
		c.Header("Etag", revision)
	}
	c.File(filepath.Join(config.DataPath(), c.Param("name")))
}
