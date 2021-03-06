package vm

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/wanghongfei/mini-jvm/utils"
	"github.com/wanghongfei/mini-jvm/vm/accflag"
	"github.com/wanghongfei/mini-jvm/vm/bcode"
	"github.com/wanghongfei/mini-jvm/vm/class"
	"reflect"
	"strings"
	"sync"
)

// 解释执行引擎
type InterpretedExecutionEngine struct {
	miniJvm *MiniJvm

	// methodStack *MethodStack
}

func (i *InterpretedExecutionEngine) Execute(def *class.DefFile, methodName string) error {
	return i.ExecuteWithFrame(def, methodName, "([Ljava/lang/String;)V", nil, false)
}

func (i *InterpretedExecutionEngine) ExecuteWithDescriptor(def *class.DefFile, methodName, descriptor string) error {
	return i.ExecuteWithFrame(def, methodName, descriptor, nil, false)
}

func (i *InterpretedExecutionEngine) ExecuteWithFrame(def *class.DefFile, methodName string, methodDescriptor string, lastFrame *MethodStackFrame, queryVTable bool) error {
	// fmt.Printf("[DEBUG] %v: %v\n", methodName, methodDescriptor)
	utils.LogInfoPrintf("execute method %s:%s", methodName, methodDescriptor)

	// 查找方法
	method, err := i.findMethod(def, methodName, methodDescriptor, queryVTable)
	if nil != err {
		return fmt.Errorf("failed to find method: %w", err)
	}
	// 因为method有可能是在父类中找到的，因此需要更新一下def到method对应的def
	def = method.DefFile

	// 解析访问标记
	flagMap := accflag.ParseAccFlags(method.AccessFlags)
	// 是native方法
	if _, ok := flagMap[accflag.Native]; ok {
		// 查本地方法表
		nativeFunc, methodArgCount := i.miniJvm.NativeMethodTable.FindMethod(def.FullClassName, methodName, methodDescriptor)
		if nil == nativeFunc {
			// 该本地方法尚未被支持
			return fmt.Errorf("unsupported native method '%s'", method)
		}

		// 调用本地方法时, 固定第一个参数时JVM指针
		nativeSpecialArgJvm := i.miniJvm
		// 第二个参数是方法接收者
		var nativeSpecialArgReceiver interface{}

		// 是否为static方法
		_, isStatic := flagMap[accflag.Static]
		if !isStatic {
			// 取出this引用
			if 0 == methodArgCount {
				// 方法没有参数, 此时栈顶是接收者引用
				nativeSpecialArgReceiver, _ = lastFrame.opStack.PopReference()
			}
			//obj, _ := lastFrame.opStack.GetTopObject()
			//nativeSpecialArgReceiver = obj

		} else {
			// 接收者是class本身
			nativeSpecialArgReceiver = def
		}

		// 构造参数数组, 长度是方法参数个数 + 2
		argCount := methodArgCount + 2
		args := make([]interface{}, argCount)
		// 从操作数栈取出methodCount个参数
		for ix := 0; ix < methodArgCount; ix++ {
			arg, _ := lastFrame.opStack.Pop()
			args[ix] = arg
			// args = append(args, arg)
		}

		// 填充前2个固定参数
		args[argCount - 1] = nativeSpecialArgJvm
		args[argCount - 2] = nativeSpecialArgReceiver

		// 因为出栈顺序跟实际参数顺序是相反的, 所以需要反转数组
		for ix := 0; ix < argCount / 2; ix++ {
			args[ix], args[argCount - 1 - ix] = args[argCount - 1 - ix], args[ix]
		}

		if strings.HasPrefix(methodName, "print") {
			i.miniJvm.DebugPrintHistory = append(i.miniJvm.DebugPrintHistory, args[2:]...)
		}

		// 调用go函数
		funcRet := nativeFunc(args...)
		if nil != funcRet {
			// native函数有返回值
			// 返回值压入上一个栈中
			lastFrame.opStack.Push(funcRet)
		}

		return nil
	}

	// 提取code属性
	codeAttr, err := i.findCodeAttr(method)
	if nil != err {
		return fmt.Errorf("failed to extract code attr: %w", err)
	}

	// 创建栈帧
	frame := newMethodStackFrame(int(codeAttr.MaxStack), int(codeAttr.MaxLocals))

	// 如果没有上层栈帧
	if nil == lastFrame && "main" == methodName {
		// main方法, 提取命令行参数, 构造String[]
		cmdArgs, _ := class.NewArray(len(i.miniJvm.CmdArgs), 0)
		cmdArgs.Array.Type = 0

		// 构造String[]数组
		cmdArgs.Array.Data = make([]interface{}, 0, len(i.miniJvm.CmdArgs))
		// stringRefs := make([]*class.Reference, 0, len(os.Args))
		// 构造String对象
		for _, goArg := range i.miniJvm.CmdArgs {
			strRune := []rune(goArg)
			stringRef, _ := class.NewStringObject(strRune, i.miniJvm.MethodArea)
			cmdArgs.Array.Data = append(cmdArgs.Array.Data, stringRef)
		}

		// 把String[]参数放在本地变量表中
		frame.localVariablesTable[0] = cmdArgs

	} else {
		// 传参
		// 判断是不是static方法
		var localVarStartIndexOffset int
		_, isStatic := flagMap[accflag.Static]
		if isStatic {
			// 如果是static方法, 则参数列表从本地变量表的0开始塞入
			localVarStartIndexOffset = 0

		} else {
			// 如果不是static方法, 则参数列表从本地变量表的1开始塞入
			localVarStartIndexOffset = 1

		}

		// 取出方法描述符
		descriptor := def.ConstPool[method.DescriptorIndex].(*class.Utf8InfoConst).String()
		// 解析描述符
		argDespList, _ := class.ParseMethodDescriptor(descriptor)
		// 临时保存参数列表
		argList := make([]interface{}, 0, len(argDespList))
		// 按参数数量出栈, 取出参数
		for _, arg := range argDespList {
			// 是int/char参数
			if "I" == arg || "C" == arg || "Ljava/lang/String" == arg || "[C" == arg {
				// 从上一个栈帧中出栈, 保存到新栈帧的localVarTable中
				op, _ := lastFrame.opStack.Pop()
				argList = append(argList, op)

				// frame.localVariablesTable[ix + localVarStartIndexOffset] = op

			} else {
				return fmt.Errorf("unsupported argument descriptor '%s' in '%s'", arg, descriptor)
			}
		}

		// 反转参数列表(因出栈顺序跟实际参数顺序相反)
		for ix := 0; ix < len(argList) / 2; ix++ {
			argList[ix], argList[len(argList) - 1 - ix] = argList[len(argList) - 1 - ix], argList[ix]
		}

		// 放入变量曹
		for ix, arg := range argList {
			frame.localVariablesTable[ix + localVarStartIndexOffset] = arg
		}

		if !isStatic {
			// 将this引用塞入0的位置
			obj, _ := lastFrame.opStack.PopReference()
			frame.localVariablesTable[0] = obj
		}

		// 是否有同步关键字
		if _, ok := flagMap[accflag.Synchronized]; ok {
			// 决定用哪个锁
			var lock *sync.Mutex
			// 如果是静态方法
			if _, isStatic := flagMap[accflag.Static]; isStatic {
				// 锁的是class
				lock = &def.Monitor
			} else {
				lock = &(frame.localVariablesTable[0].(*class.Reference).Monitor)
			}

			defer func() {
				lock.Unlock()
			}()

			// 上锁
			lock.Lock()
		}
	}



	// 执行字节码
	return i.executeInFrame(def, codeAttr, frame, lastFrame, methodName, methodDescriptor)
}

func (i *InterpretedExecutionEngine) executeWithFrameAndExceptionAdvice(def *class.DefFile, methodName string,
	methodDescriptor string, lastFrame *MethodStackFrame, queryVTable bool, codeAttr *class.CodeAttr) error {

	// 执行方法
	err := i.ExecuteWithFrame(def, methodName, methodDescriptor, lastFrame, queryVTable)
	// 判断是否抛出了异常到此层面
	if exceptionErr, ok := err.(*ExceptionThrownError); ok {
		// 查异常表修改pc
		return i.athrowJumpToTargetPc(def, lastFrame, codeAttr,
			exceptionErr.ExceptionRef.Object.DefFile.FullClassName, exceptionErr.ExceptionRef)
	}

	return err
}

func (i *InterpretedExecutionEngine) executeInFrame(def *class.DefFile, codeAttr *class.CodeAttr, frame *MethodStackFrame, lastFrame *MethodStackFrame, methodName string, methodDescriptor string) error {

	isWideStatus := false
	for {
		// 取出pc指向的字节码
		byteCode := codeAttr.Code[frame.pc]
		// fmt.Printf("[DEBUG] byte code: %v\n", bcode.ToName(byteCode))
		utils.LogInfoPrintf("execute byte code: %v", bcode.ToName(byteCode))

		exitLoop := false

		// 执行
		switch byteCode {
		case bcode.Aconstnull:
			frame.opStack.Push(nil)
		case bcode.Iconst0:
			// 将x压栈
			frame.opStack.Push(0)
		case bcode.Iconst1:
			frame.opStack.Push(1)
		case bcode.Iconst2:
			frame.opStack.Push(2)
		case bcode.Iconst3:
			frame.opStack.Push(3)
		case bcode.Iconst4:
			frame.opStack.Push(4)
		case bcode.Iconst5:
			frame.opStack.Push(5)

		case bcode.Iaload:
			// 将int型数组指定索引的值推送至栈顶
			// Operand Stack
			//..., arrayref, index →
			//..., value
			arrIndex, _ := frame.opStack.PopInt()
			arrRef, _ := frame.opStack.PopReference()
			frame.opStack.Push(arrRef.Array.Data[arrIndex])

		case bcode.Aaload:
			// 将引用类型的数组指定索引值压栈
			// Operand Stack
			//..., arrayref, index →
			//..., value
			arrIndex, _ := frame.opStack.PopInt()
			arrRef, _ := frame.opStack.PopReference()
			frame.opStack.Push(arrRef.Array.Data[arrIndex])

		case bcode.Caload:
			// 将char型数组指定索引的值推送至栈顶
			// Operand Stack
			//..., arrayref, index →
			//..., value
			arrIndex, _ := frame.opStack.PopInt()
			arrRef, _ := frame.opStack.PopReference()
			frame.opStack.Push(arrRef.Array.Data[arrIndex])

		case bcode.Istore1:
			// 将栈顶int型数值存入第二个本地变量
			top, _ := frame.opStack.PopInt()
			frame.localVariablesTable[1] = top
		case bcode.Istore2:
			// 将栈顶int型数值存入第3个本地变量
			top, _ := frame.opStack.PopInt()
			frame.localVariablesTable[2] = top
		case bcode.Istore3:
			// 将栈顶int型数值存入第4个本地变量
			top, _ := frame.opStack.PopInt()
			frame.localVariablesTable[3] = top

		case bcode.Lstore1:
			// 将栈顶long型数值存入本地变量
			top, _ := frame.opStack.Pop()
			frame.localVariablesTable[1] = top

		case bcode.Iload:
			// Load int from local variable
			// ilaod index
			index := codeAttr.Code[frame.pc + 1]
			frame.pc++

			frame.opStack.Push(frame.localVariablesTable[index])
		case bcode.Iload0:
			// 将第1个slot中的值压栈
			frame.opStack.Push(frame.localVariablesTable[0])
		case bcode.Iload1:
			frame.opStack.Push(frame.localVariablesTable[1])
		case bcode.Iload2:
			frame.opStack.Push(frame.localVariablesTable[2])
		case bcode.Iload3:
			frame.opStack.Push(frame.localVariablesTable[3])

		case bcode.Aload:
			index := codeAttr.Code[frame.pc + 1]
			frame.pc++

			frame.opStack.Push(frame.localVariablesTable[index])
		case bcode.Aload0:
			// 将第一个引用类型本地变量推送至栈顶
			ref := frame.GetLocalTableObjectAt(0)
			frame.opStack.Push(ref)
		case bcode.Aload1:
			ref := frame.GetLocalTableObjectAt(1)
			frame.opStack.Push(ref)
		case bcode.Aload2:
			// 将第3个引用类型本地变量推送至栈顶
			ref := frame.GetLocalTableObjectAt(2)
			frame.opStack.Push(ref)
		case bcode.Aload3:
			// 将第4个引用类型本地变量推送至栈顶
			ref := frame.GetLocalTableObjectAt(3)
			frame.opStack.Push(ref)

		case bcode.Istore:
			// istore index
			// ..., value →
			idx := codeAttr.Code[frame.pc + 1]
			frame.pc++

			val, _ := frame.opStack.Pop()
			frame.localVariablesTable[idx] = val

		case bcode.Astore:
			idx := codeAttr.Code[frame.pc + 1]
			frame.pc++

			val, _ := frame.opStack.Pop()
			frame.localVariablesTable[idx] = val
		case bcode.Astore0:
			// 将栈顶引用型数值存入本地变量
			ref, _ := frame.opStack.Pop()
			frame.localVariablesTable[0] = ref
		case bcode.Astore1:
			// 将栈顶引用型数值存入本地变量
			ref, _ := frame.opStack.Pop()
			frame.localVariablesTable[1] = ref
		case bcode.Astore2:
			ref, _ := frame.opStack.Pop()
			frame.localVariablesTable[2] = ref
		case bcode.Astore3:
			ref, _ := frame.opStack.Pop()
			frame.localVariablesTable[3] = ref

		case bcode.Iastore:
			// 在int数组中存储元素
			// stack: arrayref, index, value →
			val, _ := frame.opStack.PopInt()
			arrIndex, _ := frame.opStack.PopInt()
			arrRef, _ := frame.opStack.PopReference()

			arrRef.Array.Data[arrIndex] = val

		case bcode.Aastore:
			// 在数组中保存引用类型
			// stack: arrayref, index, value →
			val, _ := frame.opStack.Pop()
			arrIndex, _ := frame.opStack.PopInt()
			arrRef, _ := frame.opStack.PopReference()

			// todo 检查要保存的引用类型跟数组声明类型是否相符, 暂不实现
			// 保存
			arrRef.Array.Data[arrIndex] = val


		case bcode.Castore:
			// Store into char array
			// stack: arrayref, index, value →
			val, _ := frame.opStack.Pop()
			arrIndex, _ := frame.opStack.PopInt()
			arrRef, _ := frame.opStack.PopReference()
			arrRef.Array.Data[arrIndex] = val

		case bcode.Pop:
			frame.opStack.Pop()

		case bcode.Ldc:
			// 将int、float或String类型常量值从常量池中推送至栈顶
			// format: ldc byte
			err := i.bcodeLdc(def, frame, codeAttr)
			if nil != err {
				return fmt.Errorf("failed to execute 'ldc': %w", err)
			}

			//// 取出常量池数据项
			//strConst := def.ConstPool[codeAttr.Code[frame.pc + 1]].(*class.StringInfoConst)
			//frame.pc++
			//// 取出string字面值
			//strVal := def.ConstPool[strConst.StringIndex].(*class.Utf8InfoConst).String()
			//
			//strRef, err := class.NewStringObject([]rune(strVal), i.miniJvm.MethodArea)
			//if nil != err {
			//	return fmt.Errorf("failed to execute 'ldc':%w", err)
			//}
			//
			//// 入栈
			//frame.opStack.Push(strRef)


		case bcode.Dup:
			// 复制栈顶数值并将复制值压入栈顶
			top, _ := frame.opStack.GetTop()
			frame.opStack.Push(top)

		case bcode.Iadd:
			// 取出栈顶2元素，相加，入栈
			op1, _ := frame.opStack.PopInt()
			op2, _ := frame.opStack.PopInt()
			sum := op1 + op2
			frame.opStack.Push(sum)

		case bcode.Bipush:
			// 将单字节的常量值(-128~127)推送至栈顶
			num := int8(codeAttr.Code[frame.pc + 1])
			frame.opStack.Push(int(num))
			frame.pc++

		case bcode.Sipush:
			// 将一个短整型常量(-32768~32767)推送至栈顶
			twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
			frame.pc += 2

			var op int16
			err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &op)
			if nil != err {
				return fmt.Errorf("failed to read offset for sipush: %w", err)
			}

			frame.opStack.Push(int(op))

		case bcode.Ifle:
			// 当栈顶int型数值小于等于0时跳转
			err := i.bcodeIfCompZero(frame, codeAttr, func(op1 int, op2 int) bool {
				return op1 <= op2
			})

			if nil != err {
				return fmt.Errorf("failed to execute 'ifle': %w", err)
			}
		case bcode.Iflt:
			// 当栈顶int型数值小于0时跳转
			err := i.bcodeIfCompZero(frame, codeAttr, func(op1 int, op2 int) bool {
				return op1 < op2
			})

			if nil != err {
				return fmt.Errorf("failed to execute 'iflt': %w", err)
			}
		case bcode.Ifge:
			// >= 0
			err := i.bcodeIfCompZero(frame, codeAttr, func(op1 int, op2 int) bool {
				return op1 >= op2
			})

			if nil != err {
				return fmt.Errorf("failed to execute 'ifge': %w", err)
			}
		case bcode.Ifgt:
			// > 0
			err := i.bcodeIfCompZero(frame, codeAttr, func(op1 int, op2 int) bool {
				return op1 > op2
			})

			if nil != err {
				return fmt.Errorf("failed to execute 'ifgt': %w", err)
			}
		case bcode.Ifne:
			// != 0
			err := i.bcodeIfCompZero(frame, codeAttr, func(op1 int, op2 int) bool {
				return op1 != op2
			})

			if nil != err {
				return fmt.Errorf("failed to execute 'ifne': %w", err)
			}
		case bcode.Ifeq:
			// == 0
			err := i.bcodeIfCompZero(frame, codeAttr, func(op1 int, op2 int) bool {
				return op1 == op2
			})

			if nil != err {
				return fmt.Errorf("failed to execute 'ifeq': %w", err)
			}

		case bcode.Ificmpgt:
			// 比较栈顶两int型数值大小, 当结果大于0时跳转
			err := i.bcodeIfComp(frame, codeAttr, func(op1 int, op2 int) bool {
				return op2 - op1 > 0
			})
			if nil != err {
				return fmt.Errorf("failed to execute 'ificmpgt': %w", err)
			}
		case bcode.Ificmple:
			// 比较栈顶两int型数值大小, 当结果<=0时跳转
			err := i.bcodeIfComp(frame, codeAttr, func(op1 int, op2 int) bool {
				// fmt.Printf("%v compare %v\n", op1, op2)
				return op2 - op1 <= 0
			})
			if nil != err {
				return fmt.Errorf("failed to execute 'ificmple': %w", err)
			}
		case bcode.Ificmplt:
			// 比较栈顶两int型数值大小, 当结果小于0时跳转
			err := i.bcodeIfComp(frame, codeAttr, func(op1 int, op2 int) bool {
				return op2 - op1 < 0
			})
			if nil != err {
				return fmt.Errorf("failed to execute 'ificmplt': %w", err)
			}
		case bcode.Ificmpge:
			// 比较栈顶两int型数值大小, 当结果大于等于0时跳转
			err := i.bcodeIfComp(frame, codeAttr, func(op1 int, op2 int) bool {
				return op2 - op1 >= 0
			})
			if nil != err {
				return fmt.Errorf("failed to execute 'ificmpge': %w", err)
			}
		case bcode.Ificmpeq:
			// 比较栈顶两int型数值大小, 当结果等于0时跳转
			err := i.bcodeIfComp(frame, codeAttr, func(op1 int, op2 int) bool {
				return op2 - op1 == 0
			})
			if nil != err {
				return fmt.Errorf("failed to execute 'ificmpeq': %w", err)
			}
		case bcode.Ificmpne:
			// 比较栈顶两int型数值大小, 当结果!=0时跳转
			err := i.bcodeIfComp(frame, codeAttr, func(op1 int, op2 int) bool {
				return op2 != op1
			})
			if nil != err {
				return fmt.Errorf("failed to execute 'ificmpne': %w", err)
			}
		case bcode.Ifacmpne:
			// 比较栈顶两个引用不相等, 不相等就跳转
			x, _ := frame.opStack.Pop()
			y, _ := frame.opStack.Pop()

			// 跳转的偏移量
			twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
			var offset int16
			err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &offset)
			if nil != err {
				return fmt.Errorf("failed to read offset for if_icmpgt: %w", err)
			}

			if x != y {
				frame.pc = frame.pc + int(offset) - 1

			} else {
				frame.pc += 2
			}

		case bcode.Ifnonnull:
			// Operand Stack
			//..., value →
			x, _ := frame.opStack.Pop()

			// 跳转的偏移量
			twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
			var offset int16
			err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &offset)
			if nil != err {
				return fmt.Errorf("failed to read offset for if_icmpgt: %w", err)
			}

			if !reflect.ValueOf(x).IsNil() {
				frame.pc = frame.pc + int(offset) - 1

			} else {
				frame.pc += 2
			}

		case bcode.Ifacmpeq:
			// 比较栈顶两个引用相等, 相等就跳转
			x, _ := frame.opStack.Pop()
			y, _ := frame.opStack.Pop()

			// 跳转的偏移量
			twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
			var offset int16
			err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &offset)
			if nil != err {
				return fmt.Errorf("failed to read offset for if_icmpgt: %w", err)
			}

			if x == y {
				frame.pc = frame.pc + int(offset) - 1

			} else {
				frame.pc += 2
			}


		case bcode.Isub:
			// ..., value1, value2 →
			// The int result is value1 - value2. The result is pushed onto the operand stack.
			val2, _ := frame.opStack.PopInt()
			val1, _ := frame.opStack.PopInt()
			val := val1 - val2

			frame.opStack.Push(val)

		case bcode.Ishl:
			// Operand Stack
			//..., value1, value2 →
			//
			//..., result

			// Both value1 and value2 must be of type int.
			// The values are popped from the operand stack.
			// An int result is calculated by shifting value1 left by s bit positions,
			// where s is the value of the low 5 bits of value2.
			// The result is pushed onto the operand stack.
			val2, _ := frame.opStack.PopInt()
			val1, _ := frame.opStack.PopInt()

			shift := val2 & 0x1bb
			val1 = val1 << shift

			frame.opStack.Push(val1)

		case bcode.Iinc:
			// 将第op1个slot的变量增加op2
			// iinc  byte constbyte
			if !isWideStatus {
				op1 := codeAttr.Code[frame.pc + 1]
				op2 := int8(codeAttr.Code[frame.pc + 2])
				frame.pc += 2

				frame.localVariablesTable[op1] = frame.GetLocalTableIntAt(int(op1)) + int(op2)

			} else {
				// wide iinc byte1 byte2 constbyte1 constbyte2
				twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
				var localVarIndex uint16
				err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &localVarIndex)
				if nil != err {
					return fmt.Errorf("failed to read local_var_index for iinc_w: %w", err)
				}


				twoByteNum = codeAttr.Code[frame.pc + 1 + 2 : frame.pc + 1 + 2 + 2]
				var num int16
				err = binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &num)
				if nil != err {
					return fmt.Errorf("failed to read byte12 for iinc_w: %w", err)
				}

				frame.pc += 4

				newVal := frame.GetLocalTableIntAt(int(localVarIndex)) + int(num)
				frame.localVariablesTable[localVarIndex] = newVal

				isWideStatus = false
			}

		case bcode.Arraylength:
			// Operand Stack
			//..., arrayref →
			//..., length
			arrRef, _ := frame.opStack.PopReference()
			if nil == arrRef.Array {
				fmt.Println("nil")
			}
			val := len(arrRef.Array.Data)
			frame.opStack.Push(val)


		case bcode.New:
			// 创建一个对象, 并将其引用值压入栈顶
			twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
			frame.pc += 2

			var classCpIndex uint16
			err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &classCpIndex)
			if nil != err {
				return fmt.Errorf("failed to read class_cp_index for 'new': %w", err)
			}

			// 常量池中找出引用的class信息
			classCp := def.ConstPool[classCpIndex].(*class.ClassInfoConstInfo)
			// 目标class全名
			targetClassFullName := def.ConstPool[classCp.FullClassNameIndex].(*class.Utf8InfoConst).String()
			// 加载
			targetDefClass, err := i.miniJvm.MethodArea.LoadClass(targetClassFullName)
			if nil != err {
				return fmt.Errorf("failed to load class for '%s': %w", targetClassFullName, err)
			}
			// new
			obj, err := class.NewObject(targetDefClass, i.miniJvm.MethodArea)
			if nil != err {
				return fmt.Errorf("failed to new object for '%s': %w", targetClassFullName, err)
			}
			// 压栈
			frame.opStack.Push(obj)


		case bcode.Goto:
			// 跳转
			twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
			var offset int16
			err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &offset)
			if nil != err {
				return fmt.Errorf("failed to read pc offset for 'goto': %w", err)
			}

			frame.pc = frame.pc + int(offset) - 1

		case bcode.Invokestatic:
			// 调用静态方法
			err := i.invokeStatic(def, frame, codeAttr)
			if nil != err {
				return fmt.Errorf("failed to execute 'invokestatic': %w", err)
			}

		case bcode.Invokespecial:
			// 调用超类构建方法, 实例初始化方法, 私有方法
			err := i.invokeSpecial(def, frame, codeAttr)
			if nil != err {
				return fmt.Errorf("failed to execute 'invokespecial': %w", err)
			}

		case bcode.Invokevirtual:
			// public method
			err := i.invokeVirtual(def, frame, codeAttr)
			if nil != err {
				return fmt.Errorf("failed to execute 'invokevirtual': %w", err)
			}

		case bcode.Invokeinterface:
			// invokeinterface
			// indexbyte1
			// indexbyte2
			// count
			// 0
			err := i.invokeInterface(def, frame, codeAttr)
			if nil != err {
				return fmt.Errorf("failed to execute 'invokeinterface': %w", err)
			}

		case bcode.Getstatic:
			// format: getstatic byte1 byte2
			// Operand Stack
			// ..., →
			// ..., value
			err := i.bcodeGetStatic(def, frame, codeAttr)
			if nil != err {
				return fmt.Errorf("failed to execute 'getstatic': %w", err)
			}

		case bcode.Putstatic:
			// putstatic b1 b2
			// Operand Stack
			//..., value →
			//...
			err := i.bcodePutStatic(def, frame, codeAttr)
			if nil != err {
				return fmt.Errorf("failed to execute 'putstatic': %w", err)
			}


		case bcode.Putfield:
			// 对象字段赋值
			twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
			frame.pc += 2

			var fieldRefCpIndex uint16
			err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &fieldRefCpIndex)
			if nil != err {
				return fmt.Errorf("failed to read field_ref_cp_index: %w", err)
			}

			// 取出引用的字段
			fieldRef := def.ConstPool[fieldRefCpIndex].(*class.FieldRefConstInfo)
			// 取出字段名
			nameAndType := def.ConstPool[fieldRef.NameAndTypeIndex].(*class.NameAndTypeConst)
			fieldName := def.ConstPool[nameAndType.NameIndex].(*class.Utf8InfoConst).String()

			// 赋值
			val, _ := frame.opStack.Pop()
			ref, _ := frame.opStack.PopReference()
			ref.Object.ObjectFields[fieldName].FieldValue = val

		case bcode.GetField:
			// 获取指定对象的实例域, 并将其压入栈顶
			twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
			frame.pc += 2

			var fieldRefCpIndex uint16
			err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &fieldRefCpIndex)
			if nil != err {
				return fmt.Errorf("failed to read field_ref_cp_index: %w", err)
			}

			// 取出引用的字段
			fieldRef := def.ConstPool[fieldRefCpIndex].(*class.FieldRefConstInfo)
			// 取出字段名
			nameAndType := def.ConstPool[fieldRef.NameAndTypeIndex].(*class.NameAndTypeConst)
			fieldName := def.ConstPool[nameAndType.NameIndex].(*class.Utf8InfoConst).String()

			// 取出引用的对象
			targetObjRef, _ := frame.opStack.PopReference()

			// 读取
			field := targetObjRef.Object.ObjectFields[fieldName]
			val := field.FieldValue
			// 压栈
			frame.opStack.Push(val)

		case bcode.Newarray:
			// newarray type(byte)
			// 取出数组类型
			arrayType := codeAttr.Code[frame.pc + 1]
			frame.pc += 1

			// 栈顶元素为数组长度
			arrLen, _ := frame.opStack.PopInt()

			arrRef, err := class.NewArray(arrLen, arrayType)
			if nil != err {
				return fmt.Errorf("failed to execute 'newarray': %w", err)
			}

			// 数组引用入栈
			frame.opStack.Push(arrRef)

		case bcode.Anewarray:
			// anewarray
			// indexbyte1
			// indexbyte2

			// Operand Stack
			//..., count →
			//
			//..., arrayref

			// (indexbyte1 << 8) | indexbyte2 组合成常量池下标
			twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
			frame.pc += 2
			var objectRefCpIndex uint16
			err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &objectRefCpIndex)
			if nil != err {
				return fmt.Errorf("failed to read field_ref_cp_index: %w", err)
			}

			// 取出类型引用常量
			// 暂时只支持class类型, 不支持interface类型
			classInfoConst := def.ConstPool[objectRefCpIndex].(*class.ClassInfoConstInfo)
			// 取出类名
			className := def.ConstPool[classInfoConst.FullClassNameIndex].(*class.Utf8InfoConst).String()
			// 取出数组容量
			arrCap, _ := frame.opStack.PopInt()

			// 创建数组
			arrRef, _ := class.NewObjectArray(arrCap, className)
			// 入栈
			frame.opStack.Push(arrRef)

		case bcode.Athrow:
			err := i.bcodeAthrow(def, frame, codeAttr)
			if nil != err {
				if _, ok := err.(*ExceptionThrownError); ok {
					return err
				}

				return fmt.Errorf("failed to execute 'athrow': %w", err)
			}

		case bcode.Monitorenter:
			i.bcodeMonitorEnter(def, frame, codeAttr)
		case bcode.Monitorexit:
			i.bcodeMonitorExit(def, frame, codeAttr)

		case bcode.Ireturn:
			// 当前栈出栈, 值压入上一个栈
			op, _ := frame.opStack.PopInt()
			lastFrame.opStack.Push(op)

			exitLoop = true

		case bcode.Areturn:
			// 当前栈出栈, 值压入上一个栈
			ref, _ := frame.opStack.PopReference()
			lastFrame.opStack.Push(ref)

			exitLoop = true

		case bcode.Return:
			// 返回
			exitLoop = true

		case bcode.Wide:
			// 加宽下一个字节码
			isWideStatus = true

		default:
			return fmt.Errorf("unsupported byte code %s", hex.EncodeToString([]byte{byteCode}))
		}

		if exitLoop {
			break
		}

		// 移动程序计数器
		frame.pc++
	}

	return nil
}

func (i *InterpretedExecutionEngine) invokeStatic(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
	frame.pc += 2

	var methodRefCpIndex uint16
	err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &methodRefCpIndex)
	if nil != err {
		return fmt.Errorf("failed to read method_ref_cp_index: %w", err)
	}

	// 取出引用的方法
	methodRef := def.ConstPool[methodRefCpIndex].(*class.MethodRefConstInfo)
	// 取出方法名
	nameAndType := def.ConstPool[methodRef.NameAndTypeIndex].(*class.NameAndTypeConst)
	methodName := def.ConstPool[nameAndType.NameIndex].(*class.Utf8InfoConst).String()
	// 描述符
	descriptor := def.ConstPool[nameAndType.DescIndex].(*class.Utf8InfoConst).String()
	// 取出方法所在的class
	classRef := def.ConstPool[methodRef.ClassIndex].(*class.ClassInfoConstInfo)
	// 取出目标class全名
	targetClassFullName := def.ConstPool[classRef.FullClassNameIndex].(*class.Utf8InfoConst).String()
	// 加载
	targetDef, err := i.miniJvm.MethodArea.LoadClass(targetClassFullName)
	if nil != err {
		return fmt.Errorf("failed to load class for '%s': %w", targetClassFullName, err)
	}

	// 调用
	return i.executeWithFrameAndExceptionAdvice(targetDef, methodName, descriptor, frame, false, codeAttr)
}

func (i *InterpretedExecutionEngine) invokeSpecial(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
	frame.pc += 2

	var methodRefCpIndex uint16
	err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &methodRefCpIndex)
	if nil != err {
		return fmt.Errorf("failed to read method_ref_cp_index: %w", err)
	}


	// 取出引用的方法
	methodRef := def.ConstPool[methodRefCpIndex].(*class.MethodRefConstInfo)
	// 取出方法名
	nameAndType := def.ConstPool[methodRef.NameAndTypeIndex].(*class.NameAndTypeConst)
	methodName := def.ConstPool[nameAndType.NameIndex].(*class.Utf8InfoConst).String()
	// 描述符
	descriptor := def.ConstPool[nameAndType.DescIndex].(*class.Utf8InfoConst).String()
	// 取出方法所在的class
	classRef := def.ConstPool[methodRef.ClassIndex].(*class.ClassInfoConstInfo)
	// 取出目标class全名
	targetClassFullName := def.ConstPool[classRef.FullClassNameIndex].(*class.Utf8InfoConst).String()
	// 加载
	targetDef, err := i.miniJvm.MethodArea.LoadClass(targetClassFullName)
	if nil != err {
		return fmt.Errorf("failed to load class for '%s': %w", targetClassFullName, err)
	}

	if "<init>" == methodName && "java/lang/String" != targetClassFullName {
		// 忽略构造器
		// 消耗一个引用
		frame.opStack.PopReference()
		return nil
	}

	// 调用
	return i.executeWithFrameAndExceptionAdvice(targetDef, methodName, descriptor, frame, false, codeAttr)
}

func (i *InterpretedExecutionEngine) invokeVirtual(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
	frame.pc += 2

	var methodRefCpIndex uint16
	err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &methodRefCpIndex)
	if nil != err {
		return fmt.Errorf("failed to read method_ref_cp_index: %w", err)
	}

	// 取出引用的方法
	methodRef := def.ConstPool[methodRefCpIndex].(*class.MethodRefConstInfo)
	// 取出方法名
	nameAndType := def.ConstPool[methodRef.NameAndTypeIndex].(*class.NameAndTypeConst)
	methodName := def.ConstPool[nameAndType.NameIndex].(*class.Utf8InfoConst).String()
	// 描述符
	descriptor := def.ConstPool[nameAndType.DescIndex].(*class.Utf8InfoConst).String()

	// 计算参数的个数
	argCount := class.ParseArgCount(descriptor)

	// 找到操作数栈中的引用, 此引用即为实际类型
	// !!!如果有目标方法有参数, 则栈顶为参数而不是方法所在的实际对象，切记!!!
	targetObjRef, _ := frame.opStack.GetObjectSkip(argCount)
	targetDef := targetObjRef.Object.DefFile



	//// 取出方法所在的class
	//classRef := def.ConstPool[methodRef.ClassIndex].(*class.ClassInfoConstInfo)
	//// 取出目标class全名
	//targetClassFullName := def.ConstPool[classRef.FullClassNameIndex].(*class.Utf8InfoConst).String()
	//// 加载
	//targetDef, err := i.miniJvm.findDefClass(targetClassFullName)
	//if nil != err {
	//	return fmt.Errorf("failed to load class for '%s': %w", targetClassFullName, err)
	//}

	// 取出栈顶对象引用
	// targetObj, _ := frame.opStack.PopReference()


	// 调用
	return i.executeWithFrameAndExceptionAdvice(targetDef, methodName, descriptor, frame, true, codeAttr)
}

func (i *InterpretedExecutionEngine) invokeInterface(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	// invokeinterface
	// indexbyte1
	// indexbyte2
	// count
	// 0

	// 读取方法引用索引
	twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
	var interfaceConstIndex int16
	err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &interfaceConstIndex)
	if nil != err {
		return fmt.Errorf("failed to read interface_const_index for 'invokeinterface': %w", err)
	}

	// 多消耗2 byte
	twoByteNum = codeAttr.Code[frame.pc + 1 + 2 : frame.pc + 1 + 2 + 2]
	var nothing int16
	err = binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &nothing)
	if nil != err {
		return fmt.Errorf("failed to read interface_const_index.nothing for 'invokeinterface': %w", err)
	}

	// 移动计数器
	frame.pc += 4

	// 取出接口方法引用
	interfaceMethodRef := def.ConstPool[interfaceConstIndex].(*class.InterfaceMethodConst)
	nameAndType := def.ConstPool[interfaceMethodRef.NameAndTypeIndex].(*class.NameAndTypeConst)

	targetMethodName := def.ConstPool[nameAndType.NameIndex].(*class.Utf8InfoConst).String()
	targetDescriptor := def.ConstPool[nameAndType.DescIndex].(*class.Utf8InfoConst).String()

	// 出栈取出对象引用
	ref, _ := frame.opStack.GetUntilObject()
	return i.executeWithFrameAndExceptionAdvice(ref.Object.DefFile, targetMethodName, targetDescriptor, frame, false, codeAttr)
}

func (i *InterpretedExecutionEngine) bcodeLdc(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	// 将int、float,String或者class从常量池中推送至栈顶
	// format: ldc byte

	// 取出常量池数据项
	constItem := def.ConstPool[codeAttr.Code[frame.pc + 1]]
	var resultRef interface{}
	switch constItem.(type) {
	case *class.StringInfoConst:
		// 是string类型, 构造string对象后入栈
		strConst := def.ConstPool[codeAttr.Code[frame.pc + 1]].(*class.StringInfoConst)
		frame.pc++
		// 取出string字面值
		strVal := def.ConstPool[strConst.StringIndex].(*class.Utf8InfoConst).String()

		strRef, err := class.NewStringObject([]rune(strVal), i.miniJvm.MethodArea)
		if nil != err {
			return fmt.Errorf("failed to execute 'ldc':%w", err)
		}

		resultRef = strRef


	case *class.ClassInfoConstInfo:
		frame.pc++

		// 是class类型, 构造class实例后入栈
		classDef, err := i.miniJvm.MethodArea.LoadClass("java/lang/Class")
		if nil != err {
			return fmt.Errorf("failed to load java/lang/Class def:%w", err)
		}

		classRef, err := class.NewObject(classDef, i.miniJvm.MethodArea)
		if nil != err {
			return fmt.Errorf("failed to create java/lang/Class object:%w", err)
		}

		resultRef = classRef

	case *class.IntegerInfoConst:
		frame.pc++

		intConst := constItem.(*class.IntegerInfoConst)
		resultRef = int(intConst.Bytes)


	default:
		return errors.New("unsupported const pool type " + reflect.TypeOf(constItem).String())
	}

	// 入栈
	frame.opStack.Push(resultRef)
	return nil
}

// 解释athrow指令
func (i *InterpretedExecutionEngine) bcodeAthrow(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	// 栈顶一定是异常对象引用
	ref, _ := frame.opStack.GetTopObject()

	// 栈顶异常全名
	thisExpInfo, _ := ref.Object.DefFile.ConstPool[ref.Object.DefFile.ThisClass].(*class.ClassInfoConstInfo)
	thisExpFullName := ref.Object.DefFile.ConstPool[thisExpInfo.FullClassNameIndex].(*class.Utf8InfoConst).String()

	return i.athrowJumpToTargetPc(def, frame, codeAttr, thisExpFullName, ref)
}

// 查异常表,修改pc为需要跳转的值;
// 如果没有找到匹配的异常，返回ExceptionThrownError
func (i *InterpretedExecutionEngine) athrowJumpToTargetPc(def *class.DefFile, frame *MethodStackFrame,
	codeAttr *class.CodeAttr, thrownExceptionFullName string, thrownExceptionRef *class.Reference) error {

	// 查异常表
	if 0 == codeAttr.ExceptionTableLength {
		// 没有异常表
		return NewExceptionThrownError(thrownExceptionRef)
	}

	// 遍历异常表
	for _, expTable := range codeAttr.ExceptionTable {
		// 确保当前pc是在范围内
		if frame.pc < int(expTable.StartPc) || frame.pc > int(expTable.EndPc) {
			continue
		}

		if 0 == expTable.CatchType {
			// 没有catch语句, 直接跳转pc
			frame.pc = int(expTable.HandlerPc) - 1
			// 清空栈
			frame.opStack.Clean()
			// 将异常引用压回
			frame.opStack.Push(thrownExceptionRef)
			return nil
		}

		// 取出目标异常类型
		targetExpInfo := def.ConstPool[expTable.CatchType].(*class.ClassInfoConstInfo)
		// 目标异常全名
		targetExpFullName := def.ConstPool[targetExpInfo.FullClassNameIndex].(*class.Utf8InfoConst).String()

		// 判断跟栈顶异常是否匹配
		if targetExpFullName == thrownExceptionFullName {
			// 修改pc实现跳转
			frame.pc = int(expTable.HandlerPc) - 1
			// 清空栈
			frame.opStack.Clean()
			// 将异常引用压回
			frame.opStack.Push(thrownExceptionRef)

			return nil
		}
	}

	// 异常表中没找到跑出的异常
	return NewExceptionThrownError(thrownExceptionRef)
}

// 读取static字段
// format: getstatic byte1 byte2
// Operand Stack
// ..., →
// ..., value
func (i *InterpretedExecutionEngine) bcodeGetStatic(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	// 静态字段在cp里的index
	twoByte := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
	var fieldCpIndex int16
	err := binary.Read(bytes.NewBuffer(twoByte), binary.BigEndian, &fieldCpIndex)
	if nil != err {
		return fmt.Errorf("failed to read static field index: %w", err)
	}

	frame.pc += 2

	// 静态字段cp信息
	fieldInfo := def.ConstPool[fieldCpIndex].(*class.FieldRefConstInfo)
	// 取出字段所属class
	targetClassInfo := def.ConstPool[fieldInfo.ClassIndex].(*class.ClassInfoConstInfo)
	// 目标class全名
	targetClassFullName := def.ConstPool[targetClassInfo.FullClassNameIndex].(*class.Utf8InfoConst).String()
	// 加载
	targetClassDef, err := i.miniJvm.MethodArea.LoadClass(targetClassFullName)
	if nil != err {
		return fmt.Errorf("failed to load target class '%s':%w", targetClassFullName, err)
	}

	// 字段nameAndType
	nameAndTypeInfo := def.ConstPool[fieldInfo.NameAndTypeIndex].(*class.NameAndTypeConst)
	fieldName := def.ConstPool[nameAndTypeInfo.NameIndex].(*class.Utf8InfoConst).String()
	// fieldDesc := def.ConstPool[nameAndTypeInfo.DescIndex].(*class.Utf8InfoConst).String()

	// 查找目标字段
	objectField := targetClassDef.ParsedStaticFields[fieldName]
	// 压栈
	frame.opStack.Push(objectField)

	return nil
}

func (i *InterpretedExecutionEngine) bcodePutStatic(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	// 静态字段在cp里的index
	twoByte := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
	var fieldCpIndex int16
	err := binary.Read(bytes.NewBuffer(twoByte), binary.BigEndian, &fieldCpIndex)
	if nil != err {
		return fmt.Errorf("failed to read static field index: %w", err)
	}

	frame.pc += 2

	// 静态字段cp信息
	fieldInfo := def.ConstPool[fieldCpIndex].(*class.FieldRefConstInfo)
	// 取出字段所属class
	targetClassInfo := def.ConstPool[fieldInfo.ClassIndex].(*class.ClassInfoConstInfo)
	// 目标class全名
	targetClassFullName := def.ConstPool[targetClassInfo.FullClassNameIndex].(*class.Utf8InfoConst).String()
	// 加载
	targetClassDef, err := i.miniJvm.MethodArea.LoadClass(targetClassFullName)
	if nil != err {
		return fmt.Errorf("failed to load target class '%s':%w", targetClassFullName, err)
	}

	// 字段nameAndType
	nameAndTypeInfo := def.ConstPool[fieldInfo.NameAndTypeIndex].(*class.NameAndTypeConst)
	fieldName := def.ConstPool[nameAndTypeInfo.NameIndex].(*class.Utf8InfoConst).String()
	// fieldDesc := def.ConstPool[nameAndTypeInfo.DescIndex].(*class.Utf8InfoConst).String()


	// 出栈
	val, _ := frame.opStack.Pop()

	// set字段
	targetClassDef.ParsedStaticFields[fieldName] = class.NewObjectField(val)

	return nil
}

func (i *InterpretedExecutionEngine) bcodeMonitorEnter(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	ref, _ := frame.opStack.PopReference()
	ref.Monitor.Lock()

	return nil
}

func (i *InterpretedExecutionEngine) bcodeMonitorExit(def *class.DefFile, frame *MethodStackFrame, codeAttr *class.CodeAttr) error {
	ref, _ := frame.opStack.PopReference()
	ref.Monitor.Unlock()

	return nil
}

func (i *InterpretedExecutionEngine) bcodeIfComp(frame *MethodStackFrame, codeAttr *class.CodeAttr, gotoJudgeFunc func(int, int) bool) error {
	// 比较栈顶两int型数值大小

	// 待比较的数
	x, _ := frame.opStack.PopInt()
	y, _ := frame.opStack.PopInt()

	// 跳转的偏移量
	twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
	var offset int16
	err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &offset)
	if nil != err {
		return fmt.Errorf("failed to read offset for if_icmpgt: %w", err)
	}

	if gotoJudgeFunc(x, y) {
		frame.pc = frame.pc + int(offset) - 1

	} else {
		frame.pc += 2
	}

	return nil
}

func (i *InterpretedExecutionEngine) bcodeIfCompZero(frame *MethodStackFrame, codeAttr *class.CodeAttr, gotoJudgeFunc func(int, int) bool) error {
	// 当栈顶int型数值小于0时跳转
	// 跳转的偏移量
	twoByteNum := codeAttr.Code[frame.pc + 1 : frame.pc + 1 + 2]
	var offset int16
	err := binary.Read(bytes.NewBuffer(twoByteNum), binary.BigEndian, &offset)
	if nil != err {
		return fmt.Errorf("failed to read offset for if_icmpgt: %w", err)
	}

	op, _ := frame.opStack.PopInt()
	if gotoJudgeFunc(op, 0) {
		frame.pc = frame.pc + int(offset) - 1

	} else {
		frame.pc += 2
	}

	return nil
}

func (i *InterpretedExecutionEngine) findCodeAttr(method *class.MethodInfo) (*class.CodeAttr, error) {
	for _, attrGeneric := range method.Attrs {
		attr, ok := attrGeneric.(*class.CodeAttr)
		if ok {
			return attr, nil
		}
	}

	// return nil, errors.New("no node attr in method")
	// native方法没有code属性
	return nil, nil
}

// 查找方法定义;
// def: 当前class定义
// methodName: 目标方法简单名
// methodDescriptor: 目标方法描述符
// queryVTable: 是否只在虚方法表中查找
func (i *InterpretedExecutionEngine) findMethod(def *class.DefFile, methodName string, methodDescriptor string, queryVTable bool) (*class.MethodInfo, error) {
	if queryVTable {
		// 直接从虚方法表中查找
		for _, item := range def.VTable {
			if item.MethodName == methodName && item.MethodDescriptor == methodDescriptor {
				return item.MethodInfo, nil
			}
		}

		return nil, fmt.Errorf("method '%s' not found in VTable", methodName)
	}

	currentClassDef := def
	for {
		//className := currentClassDef.ExtractFullClassName()
		//fmt.Println(className)
		for _, method := range currentClassDef.Methods {
			name := currentClassDef.ConstPool[method.NameIndex].(*class.Utf8InfoConst).String()
			descriptor := currentClassDef.ConstPool[method.DescriptorIndex].(*class.Utf8InfoConst).String()
			// 匹配简单名和描述符
			if name == methodName && descriptor == methodDescriptor {
				return method, nil
			}
		}

		if 0 == def.SuperClass {
			break
		}

		// 从父类中寻找
		parentClassRef := currentClassDef.ConstPool[currentClassDef.SuperClass].(*class.ClassInfoConstInfo)
		// 取出父类全名
		targetClassFullName := currentClassDef.ConstPool[parentClassRef.FullClassNameIndex].(*class.Utf8InfoConst).String()
		// 查找到Exception就止步, 目前还没有支持这个class的加载
		if "java/lang/Exception" == targetClassFullName {
			break
		}

		// 加载父类
		parentDef, err := i.miniJvm.MethodArea.LoadClass(targetClassFullName)
		if nil != err {
			return nil, fmt.Errorf("failed to load superclass '%s': %w", targetClassFullName, err)
		}

		currentClassDef = parentDef
	}


	return nil, fmt.Errorf("method '%s' not found", methodName)
}

func NewInterpretedExecutionEngine(vm *MiniJvm) *InterpretedExecutionEngine {
	return &InterpretedExecutionEngine{
		miniJvm:     vm,
		// methodStack: NewMethodStack(1024),
	}
}

