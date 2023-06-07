package core

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/science-engineering-art/kademlia-grpc/interfaces"
	"github.com/science-engineering-art/kademlia-grpc/pb"
	"github.com/science-engineering-art/kademlia-grpc/structs"
	"github.com/science-engineering-art/kademlia-grpc/utils"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
)

const (
	StoreValue = iota
	KNeartestNodes
	GetValue
)

type FullNode struct {
	pb.UnimplementedFullNodeServer
	dht *DHT
}

func NewFullNode(nodeIP string, nodePort, bootstrapPort int, storage interfaces.Persistence, isBootstrapNode bool) *FullNode {

	id, _ := NewID(nodeIP, nodePort)
	node := structs.Node{ID: id, IP: nodeIP, Port: nodePort}
	routingTable := structs.NewRoutingTable(node)
	dht := DHT{Node: node, RoutingTable: routingTable, Storage: storage}
	fullNode := FullNode{dht: &dht}

	// go func() {
	// 	for {
	// 		<-time.After(10 * time.Second)
	// 		fmt.Println("\nROUTING TABLE:")
	// 		fullNode.PrintRoutingTable()
	// 		fmt.Printf("\n")
	// 	}
	// }()

	if isBootstrapNode {
		go fullNode.bootstrap(bootstrapPort)
	} else {
		go fullNode.joinNetwork(bootstrapPort)
	}

	return &fullNode
}

// Create gRPC Server
func CreateGRPCServerFromFullNode(fullNode FullNode, grpcServerAddress string) {
	grpcServer := grpc.NewServer()

	pb.RegisterFullNodeServer(grpcServer, &fullNode)
	reflection.Register(grpcServer)

	listener, err := net.Listen("tcp", grpcServerAddress)
	if err != nil {
		log.Fatal("cannot create grpc server: ", err)
	}

	log.Printf("start gRPC server on %s", listener.Addr().String())
	err = grpcServer.Serve(listener)
	if err != nil {
		log.Fatal("cannot create grpc server: ", err)
	}
}

// newID generates a new random ID
func NewID(ip string, port int) ([]byte, error) {
	hashValue := sha1.Sum([]byte(ip + ":" + strconv.FormatInt(int64(port), 10)))
	return []byte(hashValue[:]), nil
}

func (fn *FullNode) Ping(ctx context.Context, sender *pb.Node) (*pb.Node, error) {

	// add the sender to the Routing Table
	_sender := structs.Node{
		ID:   sender.ID,
		IP:   sender.IP,
		Port: int(sender.Port),
	}
	fn.dht.RoutingTable.AddNode(_sender)

	receiver := &pb.Node{
		ID:   fn.dht.ID,
		IP:   fn.dht.IP,
		Port: int32(fn.dht.Port),
	}

	return receiver, nil
}

func (fn *FullNode) Store(stream pb.FullNode_StoreServer) error {
	//fmt.Printf("INIT Store() method\n\n")

	key := []byte{}
	buffer := []byte{}
	var init int32 = 0

	for {
		data, err := stream.Recv()
		if data == nil {
			//fmt.Printf("END Streaming\n\n")
			break
		}

		if init == 0 {
			//fmt.Printf("INIT Streaming\n\n")
			// add the sender to the Routing Table
			sender := structs.Node{
				ID:   data.Sender.ID,
				IP:   data.Sender.IP,
				Port: int(data.Sender.Port),
			}
			fn.dht.RoutingTable.AddNode(sender)
		}

		key = data.Key
		if init == data.Value.Init {
			buffer = append(buffer, data.Value.Buffer...)
			init = data.Value.End
		} else {
			//fmt.Printf("EXIT Store() method\n\n")
			return err
		}

		if err != nil {
			//fmt.Printf("EXIT Store() method\n\n")
			return err
		}
	}

	err := fn.dht.Store(key, &buffer)
	if err != nil {
		//fmt.Printf("EXIT Store() method\n\n")
		return err
	}
	//fmt.Printf("EXIT Store() method\n\n")
	return nil
}

func (fn *FullNode) FindNode(ctx context.Context, target *pb.Target) (*pb.KBucket, error) {
	// add the sender to the Routing Table
	sender := structs.Node{
		ID:   target.Sender.ID,
		IP:   target.Sender.IP,
		Port: int(target.Sender.Port),
	}
	fn.dht.RoutingTable.AddNode(sender)

	bucket := fn.dht.FindNode(&target.ID)

	return getKBucketFromNodeArray(bucket), nil
}

func (fn *FullNode) FindValue(target *pb.Target, stream pb.FullNode_FindValueServer) error {
	// add the sender to the Routing Table
	sender := structs.Node{
		ID:   target.Sender.ID,
		IP:   target.Sender.IP,
		Port: int(target.Sender.Port),
	}
	fn.dht.RoutingTable.AddNode(sender)

	value, neighbors := fn.dht.FindValue(&target.ID)
	response := pb.FindValueResponse{}

	if value == nil && neighbors != nil {
		response = pb.FindValueResponse{
			KNeartestBuckets: getKBucketFromNodeArray(neighbors),
			Value: &pb.Data{
				Init:   0,
				End:    0,
				Buffer: []byte{},
			},
		}
	} else if value != nil && neighbors == nil {
		response = pb.FindValueResponse{
			KNeartestBuckets: &pb.KBucket{Bucket: []*pb.Node{}},
			Value: &pb.Data{
				Init:   0,
				End:    int32(len(*value)),
				Buffer: *value,
			},
		}
	} else {
		return errors.New("check code because this case shouldn't be valid")
	}
	stream.Send(&response)

	return nil
}

func getKBucketFromNodeArray(nodes *[]structs.Node) *pb.KBucket {
	result := pb.KBucket{Bucket: []*pb.Node{}}
	for _, node := range *nodes {
		result.Bucket = append(result.Bucket,
			&pb.Node{
				ID:   node.ID,
				IP:   node.IP,
				Port: int32(node.Port),
			},
		)
	}
	return &result
}

func (fn *FullNode) LookUp(target []byte) ([]structs.Node, error) {

	sl := fn.dht.RoutingTable.GetClosestContacts(structs.Alpha, target, []*structs.Node{})

	contacted := make(map[string]bool)
	contacted[string(fn.dht.ID)] = true

	if len(*sl.Nodes) == 0 {
		return nil, nil
	}

	for {
		addedNodes := 0

		for i, node := range *sl.Nodes {
			if i >= structs.Alpha {
				break
			}
			if contacted[string(node.ID)] {
				continue
			}
			contacted[string(node.ID)] = true

			// get RPC client
			client := NewClientNode(node.IP, 8080)

			// function to add the received nodes into the short list
			addRecvNodes := func(recvNodes *pb.KBucket) {
				kBucket := []*structs.Node{}
				for _, pbNode := range recvNodes.Bucket {
					if !contacted[string(pbNode.ID)] {
						kBucket = append(kBucket, &structs.Node{
							ID:   pbNode.ID,
							IP:   pbNode.IP,
							Port: int(pbNode.Port),
						})
						addedNodes++
					}
				}
				sl.Append(kBucket)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			recvNodes, err := client.FindNode(ctx,
				&pb.Target{
					ID: node.ID,
					Sender: &pb.Node{
						ID:   fn.dht.ID,
						IP:   fn.dht.IP,
						Port: int32(fn.dht.Port),
					},
				},
			)
			if err != nil {
				return nil, err
			}
			addRecvNodes(recvNodes)
		}

		sl.Comparator = fn.dht.ID
		sort.Sort(sl)

		if addedNodes == 0 {
			break
		}
	}

	kBucket := []structs.Node{}

	for i, node := range *sl.Nodes {
		if i == structs.K {
			break
		}
		kBucket = append(kBucket, structs.Node{
			ID:   node.ID,
			IP:   node.IP,
			Port: node.Port,
		})
	}
	return kBucket, nil
}

func (fn *FullNode) bootstrap(port int) {
	strAddr := fmt.Sprintf("0.0.0.0:%d", port)

	addr, err := net.ResolveUDPAddr("udp4", strAddr)
	if err != nil {
		log.Fatal(err)
	}

	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Fatal(err)
	}

	buffer := make([]byte, 1024)
	defer conn.Close()

	for {
		_, rAddr, err := conn.ReadFrom(buffer)
		if err != nil {
			log.Fatal(err)
		}
		// fmt.Printf("Received %d bytes from %v\n", n, rAddr)

		go func(rAddr net.Addr) {
			connChan := make(chan net.Conn)

			go func() {
				respConn, err := net.Dial("tcp", rAddr.String())
				for err != nil {
					respConn, err = net.Dial("tcp", rAddr.String())
				}
				connChan <- respConn
			}()

			kBucket, err := fn.LookUp(buffer[:20])
			if err != nil {
				log.Fatal(err)
			}
			kBucket = append(kBucket, fn.dht.Node)

			//Convert port from byte to int
			portInt := binary.LittleEndian.Uint32(buffer[20:24])
			intVal := int(portInt)

			host, _, _ := net.SplitHostPort(rAddr.String())
			id, _ := NewID(host, intVal)
			fn.dht.RoutingTable.AddNode(structs.Node{ID: id, IP: host, Port: intVal})

			bytesKBucket, err := utils.SerializeMessage(&kBucket)
			if err != nil {
				log.Fatal(err)
			}

			respConn := <-connChan
			respConn.Write(*bytesKBucket)
		}(rAddr)
	}
}

func (fn *FullNode) joinNetwork(boostrapPort int) {
	raddr := net.UDPAddr{
		IP:   net.IPv4(255, 255, 255, 255),
		Port: boostrapPort,
	}

	conn, err := net.DialUDP("udp4", nil, &raddr)
	if err != nil {
		log.Fatal(err)
	}

	host, port, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		log.Fatal(err)
	}

	dataToSend := fn.dht.ID
	bs := make([]byte, 4)
	binary.LittleEndian.PutUint32(bs, uint32(fn.dht.Port))
	dataToSend = append(dataToSend, bs...)
	_, err = conn.Write(dataToSend)
	if err != nil {
		log.Fatal(err)
	}
	conn.Close()

	address := fmt.Sprintf("%s:%s", host, port)
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	for err != nil {
		log.Fatal(err)
	}

	lis, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		log.Fatal(err)
	}

	tcpConn, err := lis.AcceptTCP()
	if err != nil {
		log.Fatal(err)
	}

	kBucket, err := utils.DeserializeMessage(tcpConn)
	if err != nil {
		log.Fatal(err)
	}

	for i := 0; i < len(*kBucket); i++ {
		node := (*kBucket)[i]

		recvNode, err := NewClientNode(node.IP, node.Port).Ping(fn.dht.Node)
		if err != nil {
			log.Fatal(err)
		}

		if recvNode.Equal(node) {
			fn.dht.RoutingTable.AddNode(node)
		} else {
			log.Fatal(errors.New("bad ping"))
		}
	}
}

func (fn *FullNode) StoreValue(key string, data *[]byte) (string, error) {
	//fmt.Printf("INIT StoreValue() method\n\n")

	keyHash, _ := base64.RawStdEncoding.DecodeString(key)

	nearestNeighbors, err := fn.LookUp(keyHash)
	//fmt.Printf("Neartest Neighbors:\n%v\n", nearestNeighbors)
	if err != nil {
		fmt.Printf("ERROR LookUP() method\n\n")
		fmt.Printf("EXIT StoreValue() method\n\n")
		return "", err
	}

	if len(nearestNeighbors) < structs.K {
		err := fn.dht.Store(keyHash, data)
		if err != nil {
			//fmt.Printf("ERROR Store(Me)\n\n")
			//fmt.Printf("EXIT StoreValue() method\n\n")
			return "", nil
		}
		//fmt.Printf("EXIT StoreValue() method\n\n")
		return key, nil
	}

	for _, node := range nearestNeighbors {
		client := getFullNodeClient(&node.IP, &node.Port)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sender, err := client.Store(ctx)
		if err != nil {
			//fmt.Printf("ERROR Store(%v, %d) method", node.IP, node.Port)
			fmt.Println(err.Error())
		}
		//fmt.Println("data bytes", dataBytes)
		err = sender.Send(
			&pb.StoreData{
				Sender: &pb.Node{
					ID:   fn.dht.ID,
					IP:   fn.dht.IP,
					Port: int32(fn.dht.Port),
				},
				Key: keyHash,
				// leandro_driguez: tiene sentido pasar todo el archivo?
				Value: &pb.Data{
					Init:   0,
					End:    int32(len(*data)),
					Buffer: *data,
				},
			},
		)
		if err != nil {
			//fmt.Printf("ERROR SendChunck(0, %d) method\n\n", len(*data))
			//fmt.Printf("EXIT StoreValue() method\n\n")
			return "", err
		}
	}

	//fmt.Println("Stored ID: ", key, "Stored Data:", data)
	//fmt.Printf("EXIT StoreValue() method\n\n")
	return key, nil
}

func (fn *FullNode) GetValue(target string) ([]byte, error) {
	keyHash, _ := base64.RawStdEncoding.DecodeString(target)

	val, err := fn.dht.Storage.Read(keyHash)
	if err == nil {
		return *val, nil
	}

	nearestNeighbors, err := fn.LookUp(keyHash)
	if err != nil {
		return nil, nil
	}
	//fmt.Println(nearestNeighbors)
	buffer := []byte{}

	for _, node := range nearestNeighbors {
		if len(target) == 0 {
			fmt.Println("Invalid target decoding.")
			continue
		}

		client := getFullNodeClient(&node.IP, &node.Port)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		receiver, err := client.FindValue(ctx,
			&pb.Target{
				ID: keyHash,
				Sender: &pb.Node{
					ID:   fn.dht.ID,
					IP:   fn.dht.IP,
					Port: int32(fn.dht.Port),
				},
			},
		)
		if err != nil {
			fmt.Println(err.Error())
		}
		var init int32 = 0

		for {
			data, err := receiver.Recv()
			if data == nil {
				break
			}
			if init == data.Value.Init {
				buffer = append(buffer, data.Value.Buffer...)
				init = data.Value.End
			} else {
				fmt.Println(err.Error())
			}
		}
		//fmt.Println("Received value from STREAMING in GetValue():", buffer)
		// Received data
		if len(buffer) > 0 {
			break
		}
	}

	return buffer, nil
}

func getFullNodeClient(ip *string, port *int) pb.FullNodeClient {
	address := fmt.Sprintf("%s:%d", *ip, *port)
	conn, _ := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	client := pb.NewFullNodeClient(conn)
	return client
}

func (fn *FullNode) PrintRoutingTable() {
	KBuckets := fn.dht.RoutingTable.KBuckets

	for i := 0; i < len(KBuckets); i++ {
		for j := 0; j < len(KBuckets[i]); j++ {
			fmt.Println(KBuckets[i][j])
		}
	}
}
