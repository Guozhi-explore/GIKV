package msservice

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"time"

	"viewservice"
	"zkservice"

	"github.com/samuel/go-zookeeper/zk"
)

func (slavemgr *slaveMgr) BroadcastData(data []byte) {
	slavemgr.Lock()
	defer slavemgr.Unlock()

	size := len(data)
	if size == 0 {
		return
	}

	sbuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(sbuf, uint32(size))

	for _, conn := range slavemgr.connMap {
		conn.Write(sbuf)
		conn.Write(data)
	}
}

func (slavemgr *slaveMgr) AddConn(conn *net.TCPConn) int {
	slavemgr.Lock()
	defer slavemgr.Unlock()
	slavemgr.sessid++
	slavemgr.connMap[slavemgr.sessid] = conn
	return slavemgr.sessid
}

func (master *Master) init() {
	acls := zk.WorldACL(zk.PermAll)

	c, _, err0 := zk.Connect([]string{zkservice.ZkServer}, time.Second)
	if err0 != nil {
		panic(err0)
	}
	master.conn = c
	master.masterPort = masterListenPort
	master.nodeName = zkservice.RootNode
	//check root path exist
	exists, _, err1 := master.conn.Exists(zkservice.RootPath)
	if err1 != nil || exists == false {
		panic(err1)
	}

	//try get master
	master.tryMaster()

	//create own temp node
	exists, _, err1 = master.conn.Exists(zkservice.MasterProcessPath)
	if err1 != nil || exists == false {
		panic(err1)
	}
	processNode := filepath.Join(zkservice.MasterProcessPath, master.sn)
	log.Println("node now is:", processNode)
	exists, _, err1 = master.conn.Exists(processNode)
	if err1 != nil {
		panic(err1)
	}
	if !exists {
		ret, err2 := master.conn.Create(processNode, []byte{}, zk.FlagEphemeral, acls)
		if err2 != nil {
			panic(err2)
		}
		log.Println("create self node: ", ret)
	}
}

func (master *Master) initType(bt []byte) {
	// connect to proxy
	if master.initProxyConn() == false {
		log.Println("initType failed")
		return
	}

	master.sendProxy(bt)
}

func (master *Master) tryMaster() {
	if master.slv2masterConn != nil {
		master.slv2masterConn.Close()
	}

	acls := zk.WorldACL(zk.PermAll)
	masterNode := zkservice.MasterMasterNode

	_, err1 := master.conn.Create(masterNode, []byte(master.masterPort), zk.FlagEphemeral, acls)
	if err1 == nil {
		log.Println("now process is master")
		master.bmaster = true
		master.initType([]byte("master"))
		master.initMaster()
	} else {
		//panic(err1)
		master.bmaster = false
		masterByteInfo, _, err := master.conn.Get(masterNode)
		if err != nil {
			log.Println("fatal err:", err)
		}
		master.initType([]byte("slave"))
		log.Println("current process is slave, master info: ", string(masterByteInfo))
		master.initSlave2MasterConn(string(masterByteInfo))

		// add watch on master process
		exists, _, evtCh, err0 := master.conn.ExistsW(masterNode)
		if err0 != nil || !exists {
			master.tryMaster()
		} else {
			master.handleMasterDownEvt(evtCh)
		}
	}
}

func (master *Master) initMaster() {
	master.slavemgr = &slaveMgr{connMap: make(map[int]*net.TCPConn), sessid: 0}

	// listen for slaves
	tcpAddr, err1 := net.ResolveTCPAddr("tcp4", ":"+master.masterPort)
	if err1 != nil {
		panic(err1)
	}
	tcpListener, err2 := net.ListenTCP("tcp4", tcpAddr)
	if err2 != nil {
		panic(err2)
	}

	go func() {
		for {
			tcpConn, err3 := tcpListener.AcceptTCP()
			if err3 != nil {
				panic(err3)
			}
			sessid := master.slavemgr.AddConn(tcpConn)
			log.Println(fmt.Sprintf("slave client:%s has connected! sessid: %d\n", tcpConn.RemoteAddr().String(), sessid))
			defer tcpConn.Close()
		}
	}()
}

func (master *Master) initSlave2MasterConn(masterPort string) {
	var err1 error
	master.slv2masterConn, err1 = net.Dial("tcp", ":"+masterPort)
	if err1 != nil {
		log.Println("initSlave2MasterConn failed: ", err1)
		return
	}

	go func() {
		for {
			hsize := make([]byte, 4)
			if _, err := io.ReadFull(master.slv2masterConn, hsize); err != nil {
				log.Println(err)
				master.slv2masterConn.Close()
				master.slv2masterConn = nil
				return
			}

			hsval := binary.LittleEndian.Uint32(hsize)
			if hsval > packetSizeMax {
				log.Println("packet size:", hsval, ",exceed max val:", packetSizeMax)
				master.slv2masterConn.Close()
				return
			}

			hbuf := make([]byte, hsval)
			if _, err := io.ReadFull(master.slv2masterConn, hbuf); err != nil {
				log.Println("read buf err:", err)
				master.slv2masterConn.Close()
				master.slv2masterConn = nil
				return
			}
			hbufstr := string(hbuf)
			master.msgQueue <- hbufstr
			log.Println("push into queue:", hbufstr, ", size:", len(master.msgQueue))
		}
	}()
}

func (master *Master) initProxyConn() bool {
	if master.connProxy != nil {
		return true
	}

	var err1 error
	master.connProxy, err1 = net.Dial("tcp", proxyAddr)
	if err1 != nil {
		log.Println("initProxyConn failed: ", err1)
		return false
	}

	go func() {
		for {
			hsize := make([]byte, 4)
			if _, err1 := io.ReadFull(master.connProxy, hsize); err1 != nil {
				log.Println(err1)
				master.connProxy.Close()
				master.connProxy = nil
				return
			}

			hsval := binary.LittleEndian.Uint32(hsize)
			if hsval > packetSizeMax {
				log.Println("packet size: ", hsval, ",exceed max val: ", packetSizeMax)
				master.connProxy.Close()
				master.connProxy = nil
				return
			}

			hbuf := make([]byte, hsval)
			if _, err2 := io.ReadFull(master.connProxy, hbuf); err2 != nil {
				log.Println("read buf err: ", err2)
				master.connProxy.Close()
				master.connProxy = nil
				return
			}

			hbufStr := string(hbuf)
			master.msgQueue <- hbufStr
			if master.bmaster {
				master.slavemgr.BroadcastData(hbuf)
				log.Println("push into slaves queue: ", hbufStr)
			}
			log.Println("push into queue: ", hbufStr, ", size: ", len(hbufStr))
		}
	}()

	return true
}

func (master *Master) sendProxy(bt []byte) {
	btOut := bytes.NewBuffer([]byte{})
	binary.Write(btOut, binary.LittleEndian, uint32(len(bt)))
	binary.Write(btOut, binary.LittleEndian, bt)
	master.connProxy.Write(btOut.Bytes())
}

func (master *Master) onMasterDown() {
	master.tryMaster()
}

func (master *Master) handleMasterDownEvt(ch <-chan zk.Event) {
	go func(chv <-chan zk.Event) {
		e := <-chv
		log.Println("handleMasterDownEvt: ", e)
		master.onMasterDown()
	}(ch)
}

func (master *Master) getWorkInfo() {
	conn, _, err0 := zk.Connect([]string{zkservice.ZkServer}, time.Second)
	if err0 != nil {
		panic(err0)
	}

	// get primary rpc address
	address, _, err1 := conn.Get(zkservice.WorkerPrimayPath)
	if err1 != nil {
		panic(err1)
	}

	master.primaryRPCAddress = string(address)

	vshost, _, err2 := conn.Get(zkservice.WorkerViewServerPath)
	if err2 != nil {
		panic(err2)
	}

	master.vshost = string(vshost)
	master.vck = viewservice.MakeClerk("", master.vshost)
}
