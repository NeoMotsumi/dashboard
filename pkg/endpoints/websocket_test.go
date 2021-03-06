/*
Copyright 2019-2021 The Tekton Authors
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
package endpoints_test

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"strconv"

	gorillaSocket "github.com/gorilla/websocket"
	"github.com/tektoncd/dashboard/pkg/broadcaster"
	. "github.com/tektoncd/dashboard/pkg/endpoints"
	"github.com/tektoncd/dashboard/pkg/router"
	"github.com/tektoncd/dashboard/pkg/testutils"
	"github.com/tektoncd/dashboard/pkg/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type informerRecord struct {
	CRD string
	// do not access directly
	create int32
	// do not access directly
	update int32
	// do not access directly
	delete int32
}

func NewInformerRecord(kind string, updatable bool) informerRecord {
	newRecord := informerRecord{
		CRD: kind,
	}
	// Identify non-upgradable records
	if !updatable {
		newRecord.update = -1
	}
	return newRecord
}

func (i *informerRecord) Handle(event string) {
	switch event {
	case "Created":
		atomic.AddInt32(&i.create, 1)
	case "Updated":
		atomic.AddInt32(&i.update, 1)
	case "Deleted":
		atomic.AddInt32(&i.delete, 1)
	}
}

func (i *informerRecord) Create() int32 {
	return atomic.LoadInt32(&i.create)
}

func (i *informerRecord) Update() int32 {
	return atomic.LoadInt32(&i.update)
}

func (i *informerRecord) Delete() int32 {
	return atomic.LoadInt32(&i.delete)
}

// Ensures all resource types sent over websocket are received as intended
func TestWebsocketResources(t *testing.T) {
	t.Log("Enter TestLogWebsocket...")
	server, r, installNamespace := testutils.DummyServer()
	defer server.Close()

	devopsServer := strings.TrimPrefix(server.URL, "http://")
	websocketURL := url.URL{Scheme: "ws", Host: devopsServer, Path: "/v1/websockets/resources"}
	websocketEndpoint := websocketURL.String()
	const clients int = 5
	connectionDur := time.Second * 5
	var wg sync.WaitGroup

	// Remove event suffixes
	getKind := func(event string) string {
		event = strings.TrimSuffix(event, "Created")
		event = strings.TrimSuffix(event, "Updated")
		event = strings.TrimSuffix(event, "Deleted")
		return event
	}
	// CUD records
	taskRecord := NewInformerRecord(getKind(string(broadcaster.TaskCreated)), true)
	clusterTaskRecord := NewInformerRecord(getKind(string(broadcaster.ClusterTaskCreated)), true)
	extensionRecord := NewInformerRecord(getKind(string(broadcaster.ServiceExtensionCreated)), true)
	// CD records
	namespaceRecord := NewInformerRecord(getKind(string(broadcaster.NamespaceCreated)), false)

	// Route incoming socket data to correct informer
	recordMap := map[string]*informerRecord{
		taskRecord.CRD:        &taskRecord,
		clusterTaskRecord.CRD: &clusterTaskRecord,
		namespaceRecord.CRD:   &namespaceRecord,
		extensionRecord.CRD:   &extensionRecord,
	}

	for i := 1; i <= clients; i++ {
		websocketChan := clientWebsocket(websocketEndpoint, connectionDur, t)
		// Wait until connection timeout
		go func() {
			defer wg.Done()
			for {
				socketData, open := <-websocketChan
				if !open {
					return
				}
				// Get CRD kind key to grab the correct informerRecord
				messageType := getKind(string(socketData.MessageType))
				informerRecord := recordMap[messageType]
				// Get event type to update proper informerRecord field
				eventType := strings.TrimPrefix(string(socketData.MessageType), messageType)
				informerRecord.Handle(eventType)
			}
		}()
		wg.Add(1)
	}
	awaitAllClients := func() bool {
		return ResourcesBroadcaster.PoolSize() == clients
	}
	// Wait until all broadcaster has registered all clients
	awaitFatal(awaitAllClients, t, fmt.Sprintf("Expected %d clients within pool", clients))

	// CUD/CD methods should create a single informer event for each type (Create|Update|Delete)
	// Create, Update, and Delete records
	CUDTasks(r, t, installNamespace)
	CUDClusterTasks(r, t)
	CUDExtensions(r, t, installNamespace)
	// Create and Delete records
	CDNamespaces(r, t)
	// Wait until connections terminate and all subscribers have been removed from pool
	// This is our synchronization point to compare against each informerRecord
	t.Log("Waiting for clients to terminate...")
	wg.Wait()
	awaitNoClients := func() bool {
		return ResourcesBroadcaster.PoolSize() == 0
	}
	awaitFatal(awaitNoClients, t, "Pool should be empty")

	// Check that all fields have been populated
	for _, informerRecord := range recordMap {
		t.Log(informerRecord)
		creates := int(informerRecord.Create())
		updates := int(informerRecord.Update())
		deletes := int(informerRecord.Delete())
		// records without an update hook/informer
		if updates == -1 {
			if creates != clients || creates != deletes {
				t.Fatalf("CD informer %s creates[%d] and deletes[%d] not equal expected to value: %d\n", informerRecord.CRD, creates, deletes, clients)
			}
		} else {
			if creates != clients || creates != deletes || creates != updates {
				t.Fatalf("CUD informer %s creates[%d], updates[%d] and deletes[%d] not equal to expected value: %d\n", informerRecord.CRD, creates, updates, deletes, clients)
			}
		}
	}
}

// Abstract connection into a channel of broadcaster.SocketData
// Closed channel = closed connection
func clientWebsocket(websocketEndpoint string, readDeadline time.Duration, t *testing.T) <-chan broadcaster.SocketData {
	d := gorillaSocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: nil, InsecureSkipVerify: true}}
	connection, _, err := d.Dial(websocketEndpoint, nil)
	if err != nil {
		t.Fatalf("Dial error connecting to %s:, %s\n", websocketEndpoint, err)
	}
	deadlineTime := time.Now().Add(readDeadline)
	connection.SetReadDeadline(deadlineTime)
	clientChan := make(chan broadcaster.SocketData)
	go func() {
		defer close(clientChan)
		defer websocket.ReportClosing(connection)
		for {
			messageType, message, err := connection.ReadMessage()
			if err != nil {
				if !strings.Contains(err.Error(), "i/o timeout") {
					t.Error("Read error:", err)
				}
				return
			}
			if messageType == gorillaSocket.TextMessage {
				var resp broadcaster.SocketData
				if err := json.Unmarshal(message, &resp); err != nil {
					t.Error("Client Unmarshal error:", err)
					return
				}
				clientChan <- resp
				// Print out websocket data received
				t.Logf("%v\n", resp)
			}
		}
	}()
	return clientChan
}

// Checks condition until true
// Use when there is no resource to wait on
// Must be on goroutine running test function: https://golang.org/pkg/testing/#T
func awaitFatal(checkFunction func() bool, t *testing.T, message string) {
	fatalTimeout := time.Now().Add(time.Second * 5)
	for {
		if checkFunction() {
			return
		}
		if time.Now().After(fatalTimeout) {
			if message == "" {
				message = "Fatal timeout reached"
			}
			t.Fatal(message)
		}
	}
}

// CUD functions

func CUDTasks(r *Resource, t *testing.T, namespace string) {
	resourceVersion := "1"

	name := "task"
	task := testutils.GetObject("v1beta1", "Task", namespace, name, resourceVersion)
	gvr := schema.GroupVersionResource{
		Group:    "tekton.dev",
		Version:  "v1beta1",
		Resource: "tasks",
	}

	t.Log("Creating task")
	_, err := r.DynamicClient.Resource(gvr).Namespace(namespace).Create(task, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error creating task: %s: %s\n", name, err.Error())
	}

	newVersion := "2"
	task.SetResourceVersion(newVersion)
	t.Log("Updating task")
	_, err = r.DynamicClient.Resource(gvr).Namespace(namespace).Update(task, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Error updating task: %s: %s\n", name, err.Error())
	}

	t.Log("Deleting task")
	err = r.DynamicClient.Resource(gvr).Namespace(namespace).Delete(name, &metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("Error deleting task: %s: %s\n", name, err.Error())
	}
}

func CUDClusterTasks(r *Resource, t *testing.T) {
	resourceVersion := "1"

	name := "clusterTask"
	clusterTask := testutils.GetClusterObject("v1beta1", "ClusterTask", name, resourceVersion)
	gvr := schema.GroupVersionResource{
		Group:    "tekton.dev",
		Version:  "v1beta1",
		Resource: "clustertasks",
	}

	t.Log("Creating clusterTask")
	_, err := r.DynamicClient.Resource(gvr).Create(clusterTask, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error creating clusterTask: %s: %s\n", name, err.Error())
	}

	newVersion := "2"
	clusterTask.SetResourceVersion(newVersion)
	t.Log("Updating clusterTask")
	_, err = r.DynamicClient.Resource(gvr).Update(clusterTask, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Error updating clusterTask: %s: %s\n", name, err.Error())
	}

	t.Log("Deleting clusterTask")
	err = r.DynamicClient.Resource(gvr).Delete(name, &metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("Error deleting clusterTask: %s: %s\n", name, err.Error())
	}
}

func CUDExtensions(r *Resource, t *testing.T, namespace string) {
	resourceVersion := "1"

	extensionService := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "extension",
			ResourceVersion: resourceVersion,
			UID:             types.UID(strconv.FormatInt(time.Now().UnixNano(), 10)),
			Labels: map[string]string{
				router.ExtensionLabelKey: router.ExtensionLabelValue,
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "127.0.0.1",
			Ports: []corev1.ServicePort{
				{
					Port: int32(1234),
				},
			},
		},
	}

	t.Log("Creating extensionService")
	_, err := r.K8sClient.CoreV1().Services(namespace).Create(&extensionService)
	if err != nil {
		t.Fatalf("Error creating extensionService: %s: %s\n", extensionService.Name, err.Error())
	}

	newVersion := "2"
	extensionService.ResourceVersion = newVersion
	t.Log("Updating extensionService")
	_, err = r.K8sClient.CoreV1().Services(namespace).Update(&extensionService)
	if err != nil {
		t.Fatalf("Error updating extensionService: %s: %s\n", extensionService.Name, err.Error())
	}

	t.Log("Deleting extensionService")
	err = r.K8sClient.CoreV1().Services(namespace).Delete(extensionService.Name, &metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("Error deleting extensionService: %s: %s\n", extensionService.Name, err.Error())
	}
}

// CD functions

func CDNamespaces(r *Resource, t *testing.T) {
	namespace := "ns1"
	_, err := r.K8sClient.CoreV1().Namespaces().Create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	if err != nil {
		t.Fatalf("Error creating namespace '%s': %s\n", namespace, err)
	}

	t.Log("Deleting namespace")
	err = r.K8sClient.CoreV1().Namespaces().Delete(namespace, &metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("Error deleting namespace: %s: %s\n", namespace, err.Error())
	}
}
