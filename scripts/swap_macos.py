"""在同一文件系统内原子交换新旧安装路径，旧程序仅在本次安装期间用于回退。"""
import ctypes
import os
import sys


def swap_paths(left, right):
    rename = ctypes.CDLL(None, use_errno=True).renamex_np
    rename.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
    rename.restype = ctypes.c_int
    if rename(os.fsencode(left), os.fsencode(right), 2) != 0:  # Darwin RENAME_SWAP
        err = ctypes.get_errno()
        raise OSError(err, os.strerror(err), str(right))


if __name__ == '__main__':
    swap_paths(*sys.argv[1:])
