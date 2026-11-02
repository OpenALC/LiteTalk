// 入房失败 → 是否重弹密码框（#102）。
//
// 服务端对「频道需要密码但请求没带」回 forbidden，对「带了但错了」回 bad_password；
// 但 forbidden 同样用于「无 ViewChannel / 无 ConnectVoice / 非成员 / 非语音频道」。
// 早先只按 `code === 'forbidden'` 判定重弹，会让根本没有权限的用户被反复弹密码框
// ——输多少次都没用，真正原因是没有权限。
//
// 因此 forbidden 必须结合 hasPassword 判断：只有「频道确实有密码」才可能是
// 「缺密码」的语义。其余情况不弹框，由 voice store 已有的 toast 展示服务端原文
// （无权限时服务端回的就是「无权进入该频道」），不必在这里另造一句文案。

// shouldRepromptPassword 该次入房失败是否应重新弹密码框。
// hasPassword 来自本地频道快照；快照可能过期，所以 bad_password（服务端确认
// 该频道有密码且密码错）不看 hasPassword，一律重弹。
export function shouldRepromptPassword(code: string | undefined, hasPassword: boolean): boolean {
  if (code === 'bad_password') return true
  if (code === 'forbidden') return hasPassword
  return false
}
