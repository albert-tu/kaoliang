/*
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

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	//"strings"
	"context"
	"time"

	"github.com/ceph/go-ceph/rados"
	"github.com/inwinstack/kaoliang/pkg/controllers"
	"github.com/olivere/elastic"
	"github.com/satori/go.uuid"
)

func dumpOpsLogToElasticsearch(oid string) {
	fmt.Println(oid)
	return
}

func parseLogName(log string) map[string]string {
	pattern := regexp.MustCompile("^ops_(?P<Bucket>[\\w-]+)_(?P<Date>\\d{4}-\\d{2}-\\d{2}-\\d{2}).log$")

	match := pattern.FindStringSubmatch(log)
	params := make(map[string]string)
	for i, name := range pattern.SubexpNames() {
		if i > 0 && i <= len(match) {
			params[name] = match[i]
		}
	}
	return params
}

func main() {
	euid := os.Geteuid()
	if euid != 0 {
		fmt.Println("Permission denied, using root or sudo.")
		return
	}

	if len(os.Args) != 6 || os.Args[1] == "help" || os.Args[1] != "start" {
		fmt.Printf("Usage: %s [start|help] <ceph user> <pool name> <es address> <es index>\n", os.Args[0])
		return
	}

	user := os.Args[2]
	poolName := os.Args[3]

	conn, _ := rados.NewConnWithUser(user)
	conn.ReadDefaultConfigFile()
	conn.Connect()
	defer conn.Shutdown()

	ioctx, err := conn.OpenIOContext(poolName)
	if err != nil {
		fmt.Println("can not connect pool:", poolName)
		return
	}
	defer ioctx.Destroy()

	now := time.Now().Format("2006-01-02-15")
	esUrl := os.Args[4]
	esIndex := os.Args[5]
	client, err := elastic.NewClient(
		elastic.SetURL(esUrl),
	)
	if err != nil {
		fmt.Println("Can not connect to elasticsearch: ", err)
		return
	}

	ioctx.ListObjects(func(oid string) {
		stat, err := ioctx.Stat(oid)
		if err != nil {
			return
		}
		params := parseLogName(oid)
		if params["Date"] == now {
			fmt.Println("Not time to dump ops log", oid)
			return
		}
		// load ops log
		data := make([]byte, stat.Size)
		ioctx.Read(oid, data, 0)

		request := client.Bulk()
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			id, _ := uuid.NewV4()
			var log controllers.OperationLog
			line := scanner.Text()
			err := json.Unmarshal([]byte(line), &log)
			if err != nil {
				fmt.Println(err)
				continue
			}
			// add bulk insert request
			bulkReq := elastic.NewBulkIndexRequest().Index(esIndex).Type("log").Id(id.String()).Doc(log)
			request = request.Add(bulkReq)
		}
		ctx := context.Background()
		_, err = request.Do(ctx)
		if err != nil {
			fmt.Println("Bulk upload is failed %s", err)
			return
		}

		ioctx.Delete(oid)
	})
}
