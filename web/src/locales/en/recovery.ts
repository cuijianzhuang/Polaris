const recovery: Record<string, string> = {
  "备份读取": "Read archive",
  "原密钥与备份数据": "Source key and archive data",
  "隔离数据库恢复": "Restore isolated database",
  "数据与密钥迁移": "Data and key migration",
  "CA 身份": "CA identity",
  "数据库重新打开": "Database reopen",
  "线上验收": "Live acceptance",
  "恢复演练失败": "Restore drill failed",
  "隔离恢复检查通过，请查看演练记录中的验证范围": "Isolated restore checks passed. Review the scope in the drill report.",
  "恢复演练": "Restore drill",
  "恢复演练记录": "Restore drill history",
  "在临时数据库验证恢复、密钥迁移和重新打开，完成后清理。不会覆盖当前数据或连接节点；线上登录、付款、通知与授权联网检查需要另外验收。": "Restore, key migration and database reopening are checked in a temporary database that is then removed. Current data and nodes are unaffected. Live login, payments, notifications and online license checks require separate acceptance.",
  "暂无演练记录，可在备份文件旁点击恢复演练": "No drill history yet. Run a restore drill beside a backup file.",
  "检查通过": "Checks passed",
  "恢复点": "Recovery point",
  "未验证": "Not checked",
  "仅在隔离数据库中检查备份，当前数据不受影响。": "The backup is checked in an isolated database. Current data is unaffected."
}
export default recovery
