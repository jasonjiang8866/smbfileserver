package main

import (
	"net"

	"github.com/hirochachacha/go-smb2"
)

type User struct {
	serverName   string
	serverIP     string
	userName     string
	userPassword string
}

func connectSMBserver(user User) (*smb2.Session, net.Conn) {
	// connect to SMB server
	conn, err := net.Dial("tcp", user.serverIP+":445")
	if err != nil {
		panic(err)
	}

	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     user.userName,
			Password: user.userPassword,
		},
	}

	s, err := d.Dial(conn)

	if err != nil {
		panic(err)
	}

	return s, conn
}

func getMount(s *smb2.Session, shareName string) *smb2.Share {
	// connect to share
	m, err := s.Mount(shareName)
	if err != nil {
		panic(err)
	}

	return m
}

func readFile(s *smb2.Share, fileName string) []byte {
	fileBytes, err := s.ReadFile(fileName)
	if err != nil {
		panic(err)
	}
	return fileBytes
}
