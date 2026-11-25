package app

import (
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/heavy"
)

func (s *Service) heavyCommand(ctx *axi.Context, args []string) error {
	class, command, err := heavy.HeavyCommand(args)
	if err != nil {
		return axi.Usage(err.Error())
	}
	if err := heavy.Run(ctx.Context, class, command, ctx.Out, ctx.ErrOut); err != nil {
		return err
	}
	return nil
}
