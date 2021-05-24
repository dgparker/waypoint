import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

interface VariableArgs {
  variable;
}

export default class ProjectInputVariablesListComponent extends Component<VariableArgs>  {
  @tracked args;
  @tracked variable;
  @tracked isCreating: boolean;
  @tracked isEditing: boolean;

  constructor(owner: any, args: any) {
    super(owner, args);
    let { variable } = this.args;
    this.variable = variable;
    this.isEditing = false;
    this.isCreating = false;
  }

  @action
  deleteVariable(variable) {

  }

  @action
  editVariable(variable) {

  }
}
