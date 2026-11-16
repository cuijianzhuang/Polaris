export default {
  '请先应用访问策略，或恢复策略修改，再保存入站配置。': 'Apply the access policy, or revert its changes, before saving the inbound configuration.',
  '访问策略单独应用到已保存的入站和受管中转线路；底部「保存并下发」不保存来源限制、PROXY 协议或来源地址。其他入站修改仍需另行保存。': 'Apply the access policy separately to the saved inbound and its managed relay paths. “Save and push” below does not save source restrictions, PROXY protocol, or source addresses. Save other inbound changes separately.',
  '应用访问策略并下发': 'Apply access policy & deploy',
  '访问策略有未应用的修改。启用后会同时将已保存的入站设为仅通过中转提供；请确认现有线路与来源地址。': 'The access policy has unapplied changes. Enabling it also makes the saved inbound relay-only; check the existing paths and source addresses.',
  '访问策略已保存，等待节点应用。此操作不保存其他入站修改；请在「中转管理」确认各节点的应用状态。': 'Access policy saved, awaiting node application. This action does not save other inbound changes; check each node’s application status in Relay management.',
  '访问策略未保存：{error}': 'Access policy was not saved: {error}',
}
