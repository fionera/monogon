//go:build linux

// Code generated by cmd/cgo -godefs; DO NOT EDIT.
// cgo -godefs scsi_linux_defs1.go

package scsi

const (
	SG_IO                = 0x2285
	SG_DXFER_NONE        = -0x1
	SG_DXFER_TO_DEV      = -0x2
	SG_DXFER_FROM_DEV    = -0x3
	SG_DXFER_TO_FROM_DEV = -0x4
)

type sgIOHdr struct {
	Interface_id    int32
	Dxfer_direction int32
	Cmd_len         uint8
	Mx_sb_len       uint8
	Iovec_count     uint16
	Dxfer_len       uint32
	Dxferp          uintptr
	Cmdp            uintptr
	Sbp             uintptr
	Timeout         uint32
	Flags           uint32
	Pack_id         int32
	Usr_ptr         uintptr
	Status          uint8
	Masked_status   uint8
	Msg_status      uint8
	Sb_len_wr       uint8
	Host_status     uint16
	Driver_status   uint16
	Resid           int32
	Duration        uint32
	Info            uint32
	Pad_cgo_0       [4]byte
}
