package server

import (
	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
)

func (s *Server) SetPipelineOrchestrator(po *orchestrator.PipelineOrchestrator) {
	s.pipelineOrch = po
	if s.sysadminHandler != nil {
		s.sysadminHandler.PipelineOrch = po
	}
}

func (s *Server) SetPatternDAGExecutor(pe *orchestrator.PatternDAGExecutor) {
	s.patternDAGExec = pe
	if s.sysadminHandler != nil {
		s.sysadminHandler.PatternDAGExec = pe
	}
}

func (s *Server) SetMapReduceExecutor(me *orchestrator.MapReduceExecutor) {
	s.mapReduceExec = me
	if s.sysadminHandler != nil {
		s.sysadminHandler.MapReduceExec = me
	}
}

func (s *Server) SetParallelExecutor(pe *orchestrator.ParallelExecutor) {
	s.parallelExec = pe
	if s.sysadminHandler != nil {
		s.sysadminHandler.ParallelExec = pe
	}
}

func (s *Server) SetSequentialExecutor(se *orchestrator.SequentialExecutor) {
	s.sequentialExec = se
	if s.sysadminHandler != nil {
		s.sysadminHandler.SequentialExec = se
	}
}

func (s *Server) SetSwarmCoordinator(sc *orchestrator.SwarmCoordinator) {
	s.swarmCoord = sc
	if s.sysadminHandler != nil {
		s.sysadminHandler.SwarmCoord = sc
	}
}

// SetAgentController 回填 SysAdminHandler.Agent（GR-9.2-002）：该字段此前全仓
// 无赋值，预算热更新、doctor 记忆诊断、mmd-canvas 均永久失效，
// HandleSetPreference 更是直接对 nil 接口调用方法（panic）。
// 注入的是常驻单例 agent-0；记忆门面为进程共享，诊断/画布对所有会话一致。
func (s *Server) SetAgentController(a protocol.AgentController) {
	if s.sysadminHandler != nil {
		s.sysadminHandler.Agent = a
	}
}
