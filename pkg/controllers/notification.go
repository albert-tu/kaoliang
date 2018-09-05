package controllers

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/ceph/go-ceph/rados"
	"github.com/gin-gonic/gin"
	"github.com/minio/minio/cmd"
	"github.com/minio/minio/pkg/event"
	"gitlab.com/stor-inwinstack/kaoliang/pkg/config"
	"gitlab.com/stor-inwinstack/kaoliang/pkg/models"
	"gitlab.com/stor-inwinstack/kaoliang/pkg/utils"
)

var targetList *event.TargetList
var errNoSuchNotifications = errors.New("The specified bucket does not have bucket notifications")

func GetBucketNotification(c *gin.Context) {
	_, err := authenticate(c.Request)
	if err != cmd.ErrNone {
		writeErrorResponse(c, err)
	}

	bucket := c.Param("bucket")

	_, notification := c.GetQuery("notification")

	if notification {
		nConfig, err := readNotificationConfig(targetList, bucket)
		if err != nil {
			if err != errNoSuchNotifications {
				writeErrorResponse(c, cmd.ToAPIErrorCode(err))
				return
			}

			nConfig = &event.Config{}
		}

		c.XML(http.StatusOK, nConfig)
		return
	}

	ReverseProxy()(c)
}

func PutBucketNotification(c *gin.Context) {
	_, err := authenticate(c.Request)
	if err != cmd.ErrNone {
		writeErrorResponse(c, err)
	}

	bucket := c.Param("bucket")
	serverConfig := config.GetServerConfig()

	_, notification := c.GetQuery("notification")

	if notification {
		region := serverConfig.Region

		config, err := event.ParseConfig(c.Request.Body, region, targetList)
		if err != nil {
			apiErr := cmd.ErrMalformedXML
			if event.IsEventError(err) {
				apiErr = cmd.ToAPIErrorCode(err)
			}

			writeErrorResponse(c, apiErr)
			return
		}

		if err = saveNotificationConfig(config, bucket); err != nil {
			writeErrorResponse(c, cmd.ToAPIErrorCode(err))
			return
		}

		c.Status(http.StatusOK)
		return
	}

	ReverseProxy()(c)
}

func readNotificationConfig(targetList *event.TargetList, bucket string) (*event.Config, error) {
	client := models.GetCache()
	val, err := client.Get(fmt.Sprintf("config:%s", bucket)).Result()
	if err != nil {
		return nil, errNoSuchNotifications
	}

	config, err := event.ParseConfig(strings.NewReader(val), "us-east-1", targetList)

	return config, err
}

func saveNotificationConfig(conf *event.Config, bucket string) error {
	output, err := xml.Marshal(conf)
	if err != nil {
		return nil
	}

	client := models.GetCache()
	if err := client.Set(fmt.Sprintf("config:%s", bucket), output, 0).Err(); err != nil {
		return nil
	}

	return nil
}

func checkResponse(resp *http.Response, method string, statusCode int) bool {
	clientReq := resp.Request

	if clientReq.Method == method && resp.StatusCode == statusCode {
		return true
	}

	return false
}

// currently only supports path-style syntax
func getObjectName(req *http.Request) (string, string, error) {
	segments := strings.Split(req.URL.Path, "/")
	bucketName := segments[1]
	objectName := segments[2]

	return bucketName, objectName, nil
}

func sendEvent(resp *http.Response, eventType event.Name) error {
	clientReq := resp.Request
	bucketName, objectName, _ := getObjectName(clientReq)

	client := models.GetCache()
	serverConfig := config.GetServerConfig()
	nConfig, err := readNotificationConfig(targetList, bucketName)
	if err != nil {
		panic(err)
	}

	rulesMap := nConfig.ToRulesMap()
	eventTime := time.Now().UTC()

	var etag string
	if val, ok := resp.Header["Etag"]; ok {
		etag = val[0]
	}

	for targetID := range rulesMap[eventType].Match(objectName) {
		newEvent := event.Event{
			EventVersion: "2.0",
			EventSource:  "aws:s3",
			AwsRegion:    serverConfig.Region,
			EventTime:    eventTime.Format("2006-01-02T15:04:05Z"),
			EventName:    eventType,
			UserIdentity: event.Identity{
				PrincipalID: "",
			},
			RequestParameters: map[string]string{
				"sourceIPAddress": clientReq.RemoteAddr,
			},
			ResponseElements: map[string]string{
				"x-amz-request-id": resp.Header["X-Amz-Request-Id"][0],
			},
			S3: event.Metadata{
				SchemaVersion:   "1.0",
				ConfigurationID: "Config",
				Bucket: event.Bucket{
					Name: bucketName,
					OwnerIdentity: event.Identity{
						PrincipalID: "",
					},
					ARN: "",
				},
				Object: event.Object{
					Key:       objectName,
					Size:      clientReq.ContentLength,
					ETag:      etag,
					Sequencer: fmt.Sprintf("%X", eventTime.UnixNano()),
				},
			},
		}

		value, err := json.Marshal(newEvent)
		if err != nil {
			panic(err)
		}

		client.RPush(fmt.Sprintf("%s:%s:%s", targetID.Service, targetID.ID, targetID.Name), value)
	}

	return err
}

func IsAdminUserPath(path string) bool {
	return path == "/admin/user/" || path == "/admin/user"
}

func ReverseProxy() gin.HandlerFunc {
	target := utils.GetEnv("TARGET_HOST", "127.0.0.1")

	return func(c *gin.Context) {
		director := func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = target
		}

		modifyResponse := func(resp *http.Response) error {
			clientReq := resp.Request

			switch {
			case IsAdminUserPath(clientReq.URL.Path) && resp.StatusCode == 200:
				b, _ := ioutil.ReadAll(resp.Body)
				resp.Body.Close()
				go handleNfsExport(clientReq, b)
				resp.Body = ioutil.NopCloser(bytes.NewReader(b)) // put body back for client response
				return nil
			case len(clientReq.Header["X-Amz-Copy-Source"]) > 0:
				return sendEvent(resp, event.ObjectCreatedCopy)
			case len(resp.Header["Etag"]) > 0 && checkResponse(resp, "PUT", 200):
				return sendEvent(resp, event.ObjectCreatedPut)
			case checkResponse(resp, "DELETE", 204):
				return sendEvent(resp, event.ObjectRemovedDelete)
			default:
				return nil
			}
		}

		proxy := &httputil.ReverseProxy{Director: director, ModifyResponse: modifyResponse}
		proxy.ServeHTTP(c.Writer, c.Request)
	}
}

type RgwUser struct {
	UserId string   `json:"user_id"`
	Keys   []RgwKey `json:"keys"`
}

type RgwKey struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

func random(min int, max int) int {
	rand.Seed(time.Now().Unix())
	return rand.Intn(max-min) + min
}

func addNfsExport(body []byte) {
	// get user info
	var data RgwUser
	err := json.Unmarshal(body, &data)
	if err != nil {
		return
	}
	// only export when create user (same request only add key on second times)
	if len(data.Keys) > 1 {
		return
	}
	userId := data.UserId
	accessKey := data.Keys[0].AccessKey
	secretKey := data.Keys[0].SecretKey

	// connect rados
	conn, _ := rados.NewConnWithUser("admin")
	conn.ReadDefaultConfigFile()
	conn.Connect()
	defer conn.Shutdown()
	ioctx, _ := conn.OpenIOContext("nfs-ganesha")
	defer ioctx.Destroy()

	// check export is not exists
	exportObjName := fmt.Sprintf("export_%s", userId)
	// create export obj
	createNfsExportObj(ioctx, exportObjName, userId, accessKey, secretKey)
	// add export obj path to export list
	addExportPathToList(ioctx, "export", "nfs-ganesha", exportObjName)
}

func addExportPathToList(ioctx *rados.IOContext, exportName string, poolName string, exportObjName string) {
	append_lock := "export_append_lock"
	append_cookie := "export_append_cookie"
	newExport := fmt.Sprintf("%%url \"rados://%s/%s\"\n", poolName, exportObjName)
	ioctx.LockExclusive(exportName, append_lock, append_cookie, "export_append", 0, nil)
	ioctx.Append(exportName, []byte(newExport))
	ioctx.Unlock(exportName, append_lock, append_cookie)
}

func createNfsExportObj(ioctx *rados.IOContext, exportObjName, userId, accessKey, secretKey string) {
	exportId := random(1, 65535)
	exportTemp := `Export {
	Export_ID = %d;
	Path = "/";
	Pseudo = "/%s";
	Access_Type = RW;
	Protocols = 4;
	Transports = TCP;
	FSAL {
		Name = RGW; 
		User_Id = "%s"; 
		Access_Key_Id ="%s";
                Secret_Access_Key = "%s";
        }
}`
	export := fmt.Sprintf(exportTemp, exportId, userId, userId, accessKey, secretKey)
	ioctx.WriteFull(exportObjName, []byte(export))
}

func handleNfsExport(req *http.Request, body []byte) {
	_, isSubuser := req.URL.Query()["subuser"]
	_, isKey := req.URL.Query()["key"]
	_, isQuota := req.URL.Query()["quota"]
	_, isCaps := req.URL.Query()["caps"]

	// only handle user related request
	if isSubuser || isKey || isQuota || isCaps {
		return
	}
	// handle create user
	if req.Method == "PUT" {
		addNfsExport(body)
	}
}
