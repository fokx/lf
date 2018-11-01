CC=cc
CFLAGS=-O2
LIBS=-lsqlite3

OBJS=common.o ed25519.o wharrgarbl.o

all:	lf

lf:	$(OBJS)

clean:
	rm -rf *.o lf *.dSYM

FORCE:
	;